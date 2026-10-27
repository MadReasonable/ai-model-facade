package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// Open WebUI serves no models of its own: it passes requests on to the
// backends its section of config.yaml connects it to. It creates its admin
// account itself on its first start, from WEBUI_ADMIN_EMAIL and
// WEBUI_ADMIN_PASSWORD (v0.12.3 source, main.py), and takes its connections
// from its environment on every start; both come from the config, through
// the override.
//
// Its setup step makes the rest match the config through Open WebUI's API.
// It signs in as the admin, creates an API key, and keeps the key in
// control's state with the account's ID and a session token, since they
// act as the admin and still work after the config's admin changes, when
// signing in with the config's credentials no longer can. With them it
// reads the account and, if its email, name or password differs from the
// config, updates it; a password can't be read back, so it matches if
// signing in with it works. It also makes Open WebUI's workspace models,
// aliases of a connection's models with params of their own, match the
// config's list. A reload does the same on a running Open WebUI, and also
// sets its connections.

func init() {
	register(backend{
		Name: "openWebUi", Service: "open-webui", Profile: "open-webui", URL: "http://open-webui:8080",
		Health: "/health", kind: openWebUiKind{},
	})
}

type openWebUiKind struct{}

type openWebUi struct {
	basic
	Admin struct {
		Email    string `yaml:"email"`
		Password string `yaml:"password"`
		Name     string `yaml:"name"`
	} `yaml:"admin"`
	Connections []string `yaml:"connections"` // backend names
	// Models are workspace models, each an alias of one of its connections'
	// models with params of its own.
	Models []openWebUiModel `yaml:"models"`
}

// openWebUiModel is a workspace model. Open WebUI applies its params to a
// request for it, after the gateway's, and passes the request on to its
// base (v0.12.3 source, utils/payload.py).
type openWebUiModel struct {
	ID     string         `yaml:"id"`
	Base   string         `yaml:"base"` // a model of one of its connections, by its ID there
	Params map[string]any `yaml:"params"`
}

func (openWebUiKind) configure(section *yaml.Node, _ fileIndex) (configured, error) {
	var o openWebUi
	if err := decodeSection(section, &o); err != nil {
		return nil, err
	}
	var found []string
	if o.Admin.Email == "" || o.Admin.Password == "" {
		found = append(found, "needs admin, with an email and a password")
	}
	for _, name := range o.Connections {
		if b, ok := findBackend(name); !ok || b.API == "" {
			found = append(found, fmt.Sprintf("connects to '%s', which isn't a backend Open WebUI can connect to; it can connect to %s", name, joinQuoted(connectable())))
		}
	}
	seen := map[string]bool{}
	for _, m := range o.Models {
		switch {
		case m.ID == "" || strings.ContainsAny(m.ID, " \t\n"):
			found = append(found, fmt.Sprintf("model '%s' needs an id without spaces", m.ID))
		case seen[m.ID]:
			found = append(found, fmt.Sprintf("has model '%s' twice", m.ID))
		case m.Base == "":
			found = append(found, fmt.Sprintf("model '%s' needs a base, the ID of a model one of its connections offers", m.ID))
		}
		seen[m.ID] = true
	}
	return &o, problems(found)
}

// connectable lists the backends Open WebUI can connect to.
func connectable() []string {
	var names []string
	for _, b := range backends {
		if b.API != "" {
			names = append(names, b.Name)
		}
	}
	return names
}

// openWebUiConnections are the connections Open WebUI is given: Ollama's API
// for an Ollama backend, the OpenAI API for the others. The lists are never
// nil, since Open WebUI's update routes refuse a null list.
type openWebUiConnections struct {
	openAI, keys, ollama []string
}

func (o *openWebUi) connections() openWebUiConnections {
	c := openWebUiConnections{openAI: []string{}, keys: []string{}, ollama: []string{}}
	for _, name := range o.Connections {
		b, _ := findBackend(name)
		switch b.API {
		case "ollama":
			c.ollama = append(c.ollama, b.URL)
		case "openai":
			c.openAI = append(c.openAI, b.URL+"/v1")
			c.keys = append(c.keys, "none") // the backends have no key, but Open WebUI needs one per URL
		}
	}
	return c
}

// service gives Open WebUI its admin account and its connections, each kind
// turned off when there are none, since Open WebUI otherwise falls back to
// defaults.
func (o *openWebUi) service() *service {
	c := o.connections()
	return &service{Environment: map[string]string{
		"WEBUI_ADMIN_EMAIL":    o.Admin.Email,
		"WEBUI_ADMIN_PASSWORD": o.Admin.Password,
		"WEBUI_ADMIN_NAME":     o.Admin.Name,
		"OPENAI_API_BASE_URLS": strings.Join(c.openAI, ";"),
		"OPENAI_API_KEYS":      strings.Join(c.keys, ";"),
		"OLLAMA_BASE_URLS":     strings.Join(c.ollama, ";"),
		"ENABLE_OPENAI_API":    strconv.FormatBool(len(c.openAI) > 0),
		"ENABLE_OLLAMA_API":    strconv.FormatBool(len(c.ollama) > 0),
	}}
}

// reload gives the running Open WebUI the connections the config lists,
// through the admin routes its settings page uses (v0.12.3 source,
// routers/openai.py and routers/ollama.py, update_config), and makes its
// admin account and workspace models match. With ENABLE_PERSISTENT_CONFIG=False the connections
// are kept in memory, not in its database (models/config.py,
// Config.upsert), so the next start takes the environment again, which
// says the same.
func (o *openWebUi) reload(ctx context.Context, b backend, _ *env) (bool, error) {
	c, err := o.credential(ctx, b, false)
	if err != nil {
		return false, err
	}
	if c.Key == "" {
		return false, fmt.Errorf("backend '%s' has no API key yet; run its setup", b.Name)
	}
	if err := o.matchAdmin(ctx, b, c); err != nil {
		return false, err
	}
	if err := o.matchModels(ctx, b, c); err != nil {
		return false, err
	}
	conns := o.connections()
	openAI := map[string]any{
		"ENABLE_OPENAI_API":    len(conns.openAI) > 0,
		"OPENAI_API_BASE_URLS": conns.openAI,
		"OPENAI_API_KEYS":      conns.keys,
		"OPENAI_API_CONFIGS":   map[string]any{},
	}
	if err := sendJSON(ctx, http.MethodPost, b.URL+"/openai/config/update", c.Key, openAI, nil); err != nil {
		return false, fmt.Errorf("updating the OpenAI connections of '%s': %w", b.Name, err)
	}
	ollama := map[string]any{
		"ENABLE_OLLAMA_API":  len(conns.ollama) > 0,
		"OLLAMA_BASE_URLS":   conns.ollama,
		"OLLAMA_API_CONFIGS": map[string]any{},
	}
	if err := sendJSON(ctx, http.MethodPost, b.URL+"/ollama/config/update", c.Key, ollama, nil); err != nil {
		return false, fmt.Errorf("updating the Ollama connections of '%s': %w", b.Name, err)
	}
	return true, nil
}

// models lists Open WebUI's models with the state Open WebUI passes on from
// each connection: `loaded` for an Ollama model, llama-swap's status.value
// for a llama-swap one, and "unknown" otherwise (the gateway's design doc,
// Open WebUI state).
func (o *openWebUi) models(ctx context.Context, b backend) ([]model, error) {
	c, err := o.credential(ctx, b, false)
	if err != nil {
		return nil, err
	}
	if c.Key == "" {
		return nil, fmt.Errorf("backend '%s' has no API key yet; run its setup", b.Name)
	}
	var list struct {
		Data []struct {
			ID     string `json:"id"`
			Loaded *bool  `json:"loaded"`
			Status *struct {
				Value string `json:"value"`
			} `json:"status"`
		} `json:"data"`
	}
	if err := authJSON(ctx, http.MethodGet, b.URL+"/api/models?refresh=true", c.Key, &list); err != nil {
		return nil, err
	}
	var models []model
	for _, m := range list.Data {
		state := "unknown"
		switch {
		case m.Loaded != nil && *m.Loaded:
			state = "loaded"
		case m.Loaded != nil:
			state = "unloaded"
		case m.Status != nil:
			state = m.Status.Value
		}
		models = append(models, model{ID: m.ID, State: state})
	}
	return models, nil
}

func (o *openWebUi) loading(b backend) (loader, error) {
	return nil, unsupported("backend '%s' passes requests to its connections; load or unload the model on the backend behind it", b.Name)
}

func (o *openWebUi) setup() setup { return o }

func (o *openWebUi) running() string {
	return "making its API key, admin account and workspace models match the config"
}

func (o *openWebUi) run(ctx context.Context, b backend, _ *env, _ *progress) error {
	c, err := o.credential(ctx, b, true)
	if err != nil {
		return err
	}
	if err := o.matchAdmin(ctx, b, c); err != nil {
		return err
	}
	return o.matchModels(ctx, b, c)
}

// state reports the step done once there is an API key and the admin
// account's email and name and the workspace models match the config, with
// the key. The password is checked only when the step runs.
func (o *openWebUi) state(ctx context.Context, b backend, _ *env) step {
	s := step{Name: b.Name}
	c, err := o.credential(ctx, b, false)
	if err != nil {
		s.State, s.Detail = stateFailed, err.Error()
		return s
	}
	if c.Key == "" {
		s.State, s.Detail = statePending, "no API key yet"
		return s
	}
	_, differs, _, err := o.adminDiffers(ctx, b, c, false)
	if err != nil {
		s.State, s.Detail = stateFailed, err.Error()
		return s
	}
	plan, err := o.modelPlan(ctx, b, c)
	switch {
	case err != nil:
		s.State, s.Detail = stateFailed, err.Error()
	case len(differs) > 0:
		s.State, s.Detail = statePending, "the admin account differs from the config: "+strings.Join(differs, ", ")
	case !plan.matches():
		s.State, s.Detail = statePending, "the workspace models differ from the config: "+strings.Join(plan.changes, "; ")
	default:
		s.State, s.Detail, s.Key = stateDone, "the API key is in 'key'; the admin's password is checked when the step runs", c.Key
	}
	return s
}

// openWebUiCredential is what control keeps in its state to act as Open
// WebUI's admin: the API key; the account's ID, since the routes that say
// who a credential belongs to take only a session (v0.12.3 source,
// routers/auths.py, get_human_user), while the users routes take the key;
// and a session token, a second way in should the key stop working. A
// session token lasts until JWT_EXPIRES_IN, 4w by default, or until the
// password changes, which renews the account's session_stamp
// (models/auths.py, update_user_password_by_id), so control saves a new
// one whenever it signs in.
type openWebUiCredential struct {
	Account string `json:"account"`
	Key     string `json:"key"`
	Session string `json:"session"`
}

var openWebUiCredentialFile = filepath.Join(stateDir, "open-webui.json")

func savedCredential() openWebUiCredential {
	var c openWebUiCredential
	if data, err := os.ReadFile(openWebUiCredentialFile); err == nil {
		json.Unmarshal(data, &c)
	}
	return c
}

func saveCredential(c openWebUiCredential) error {
	data, err := json.Marshal(c)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return err
	}
	return os.WriteFile(openWebUiCredentialFile, data, 0o600)
}

// credential returns a credential that acts as the admin. It tries, in
// order, the saved API key, the saved session token, and signing in with
// the config's credentials; with a session from either of the last two it
// reads the account and its key, creating one if there is none and create
// is set, and saves all three. It returns an empty credential and no error
// when there is no key and create isn't set.
func (o *openWebUi) credential(ctx context.Context, b backend, create bool) (openWebUiCredential, error) {
	saved := savedCredential()
	if saved.Key != "" {
		_, err := o.account(ctx, b, saved)
		if err == nil {
			return saved, nil
		}
		if !turnedDown(err) {
			return openWebUiCredential{}, err
		}
	}
	var session struct {
		ID string `json:"id"`
	}
	token := saved.Session
	err := errors.New("no saved session")
	if token != "" {
		err = authJSON(ctx, http.MethodGet, b.URL+"/api/v1/auths/", token, &session)
		if err != nil && !turnedDown(err) {
			return openWebUiCredential{}, err
		}
	}
	if err != nil {
		token, err = o.signIn(ctx, b, o.Admin.Email, o.Admin.Password)
		if turnedDown(err) {
			return openWebUiCredential{}, fmt.Errorf("backend '%s' turns down the config's admin '%s', and control has no saved API key or session it accepts, so the account has other credentials; restore them in config.yaml, or remove the ai-model-facade-dev_open-webui volume while Open WebUI is stopped for a new account: %w", b.Name, o.Admin.Email, err)
		}
		if err != nil {
			return openWebUiCredential{}, err
		}
		if err := authJSON(ctx, http.MethodGet, b.URL+"/api/v1/auths/", token, &session); err != nil {
			return openWebUiCredential{}, err
		}
	}
	var key struct {
		APIKey string `json:"api_key"`
	}
	err = authJSON(ctx, http.MethodGet, b.URL+"/api/v1/auths/api_key", token, &key)
	if err != nil || key.APIKey == "" {
		if !create {
			return openWebUiCredential{}, nil
		}
		if err := authJSON(ctx, http.MethodPost, b.URL+"/api/v1/auths/api_key", token, &key); err != nil {
			return openWebUiCredential{}, fmt.Errorf("creating an API key on '%s': %w", b.Name, err)
		}
	}
	c := openWebUiCredential{Account: session.ID, Key: key.APIKey, Session: token}
	return c, saveCredential(c)
}

// turnedDown reports whether err is Open WebUI turning a credential down,
// rather than failing to answer: it answers 400 to a wrong password and 401
// to a key it doesn't accept.
func turnedDown(err error) bool {
	var status *statusError
	return errors.As(err, &status) && (status.code == http.StatusBadRequest || status.code == http.StatusUnauthorized || status.code == http.StatusForbidden)
}

// openWebUiAccount is a user account as GET /api/v1/users/{id} returns it.
type openWebUiAccount struct {
	Email string `json:"email"`
	Name  string `json:"name"`
}

func (o *openWebUi) account(ctx context.Context, b backend, c openWebUiCredential) (openWebUiAccount, error) {
	var a openWebUiAccount
	err := authJSON(ctx, http.MethodGet, b.URL+"/api/v1/users/"+url.PathEscape(c.Account), c.Key, &a)
	return a, err
}

// adminDiffers returns the admin account, which of its email, name and
// password differ from the config, and, when the config's password signs
// in, the session token that gave. Open WebUI keeps emails lowercased.
// Checking the password takes a sign-in, which Open WebUI limits to 15 in
// 3 minutes per email (v0.12.3 source, routers/auths.py,
// signin_rate_limiter), a limit the account's own logins share, so only a
// setup run or a reload checks it, not a read of the setup state.
func (o *openWebUi) adminDiffers(ctx context.Context, b backend, c openWebUiCredential, checkPassword bool) (openWebUiAccount, []string, string, error) {
	a, err := o.account(ctx, b, c)
	if err != nil {
		return a, nil, "", err
	}
	var differs []string
	if a.Email != strings.ToLower(o.Admin.Email) {
		differs = append(differs, "email")
	}
	if o.Admin.Name != "" && a.Name != o.Admin.Name {
		differs = append(differs, "name")
	}
	if !checkPassword {
		return a, differs, "", nil
	}
	token, err := o.signIn(ctx, b, a.Email, o.Admin.Password)
	if turnedDown(err) {
		differs = append(differs, "password")
	} else if err != nil {
		return a, nil, "", err
	}
	return a, differs, token, nil
}

// matchAdmin updates the admin account to the config's email, name and
// password where they differ, through the route Open WebUI's user settings
// use (v0.12.3 source, routers/users.py, update_user_by_id), which lets the
// primary admin update itself. It then saves a session token from signing
// in with the config's credentials, which a password change makes
// necessary, since it ends every earlier session.
func (o *openWebUi) matchAdmin(ctx context.Context, b backend, c openWebUiCredential) error {
	_, differs, token, err := o.adminDiffers(ctx, b, c, true)
	if err != nil {
		return err
	}
	if len(differs) > 0 {
		update := map[string]string{}
		for _, field := range differs {
			switch field {
			case "email":
				update["email"] = o.Admin.Email
			case "name":
				update["name"] = o.Admin.Name
			case "password":
				update["password"] = o.Admin.Password
			}
		}
		if err := sendJSON(ctx, http.MethodPost, b.URL+"/api/v1/users/"+url.PathEscape(c.Account)+"/update", c.Key, update, nil); err != nil {
			return fmt.Errorf("updating the admin account of '%s': %w", b.Name, err)
		}
		if token, err = o.signIn(ctx, b, o.Admin.Email, o.Admin.Password); err != nil {
			return err
		}
	}
	c.Session = token
	return saveCredential(c)
}

// signIn returns a session token for an account.
func (o *openWebUi) signIn(ctx context.Context, b backend, email, password string) (string, error) {
	var session struct {
		Token string `json:"token"`
	}
	if err := postJSONOut(ctx, b.URL+"/api/v1/auths/signin", map[string]string{"email": email, "password": password}, &session); err != nil {
		return "", fmt.Errorf("signing in to '%s' as '%s': %w", b.Name, email, err)
	}
	return session.Token, nil
}

// openWebUiModelPlan is what it takes to make Open WebUI's workspace models
// match the config.
type openWebUiModelPlan struct {
	create, update []openWebUiModel
	remove         []string // workspace models the config doesn't list
	changes        []string // what differs, for the setup step's detail
}

func (p openWebUiModelPlan) matches() bool {
	return len(p.create) == 0 && len(p.update) == 0 && len(p.remove) == 0
}

// modelPlan compares Open WebUI's workspace models, the stored models with
// a base, with the config's, by base and params. Every workspace model in
// this environment is control's, so one the config doesn't list is removed.
func (o *openWebUi) modelPlan(ctx context.Context, b backend, c openWebUiCredential) (openWebUiModelPlan, error) {
	var records []struct {
		ID          string         `json:"id"`
		BaseModelID string         `json:"base_model_id"`
		Params      map[string]any `json:"params"`
	}
	if err := authJSON(ctx, http.MethodGet, b.URL+"/api/v1/models/all", c.Key, &records); err != nil {
		return openWebUiModelPlan{}, err
	}
	workspace := map[string]int{}
	for i, r := range records {
		if r.BaseModelID != "" {
			workspace[r.ID] = i
		}
	}
	var p openWebUiModelPlan
	listed := map[string]bool{}
	for _, m := range o.Models {
		listed[m.ID] = true
		i, ok := workspace[m.ID]
		switch {
		case !ok:
			p.create = append(p.create, m)
			p.changes = append(p.changes, fmt.Sprintf("'%s' is missing", m.ID))
		case records[i].BaseModelID != m.Base:
			p.update = append(p.update, m)
			p.changes = append(p.changes, fmt.Sprintf("'%s' is based on '%s', not '%s'", m.ID, records[i].BaseModelID, m.Base))
		case !sameJSON(records[i].Params, m.Params):
			p.update = append(p.update, m)
			p.changes = append(p.changes, fmt.Sprintf("'%s' has other params", m.ID))
		}
	}
	for _, r := range records {
		if r.BaseModelID != "" && !listed[r.ID] {
			p.remove = append(p.remove, r.ID)
			p.changes = append(p.changes, fmt.Sprintf("'%s' isn't in the config", r.ID))
		}
	}
	return p, nil
}

// sameJSON reports whether two values encode to the same JSON, so params
// decoded from YAML compare equal to the same params decoded from Open
// WebUI's JSON. A nil map is the same as an empty one.
func sameJSON(a, b map[string]any) bool {
	normal := func(m map[string]any) any {
		if m == nil {
			m = map[string]any{}
		}
		data, _ := json.Marshal(m)
		var v any
		json.Unmarshal(data, &v)
		return v
	}
	return reflect.DeepEqual(normal(a), normal(b))
}

// matchModels creates, updates and removes Open WebUI's workspace models
// so they match the config, through the routes its workspace pages use
// (v0.12.3 source, routers/models.py). An admin may create a model whose
// base isn't offered at the moment, so the base's backend needn't run.
func (o *openWebUi) matchModels(ctx context.Context, b backend, c openWebUiCredential) error {
	plan, err := o.modelPlan(ctx, b, c)
	if err != nil {
		return err
	}
	form := func(m openWebUiModel) map[string]any {
		params := m.Params
		if params == nil {
			params = map[string]any{}
		}
		return map[string]any{"id": m.ID, "base_model_id": m.Base, "name": m.ID, "meta": map[string]any{}, "params": params}
	}
	for _, m := range plan.create {
		if err := sendJSON(ctx, http.MethodPost, b.URL+"/api/v1/models/create", c.Key, form(m), nil); err != nil {
			return fmt.Errorf("creating workspace model '%s' on '%s': %w", m.ID, b.Name, err)
		}
	}
	for _, m := range plan.update {
		if err := sendJSON(ctx, http.MethodPost, b.URL+"/api/v1/models/model/update", c.Key, form(m), nil); err != nil {
			return fmt.Errorf("updating workspace model '%s' on '%s': %w", m.ID, b.Name, err)
		}
	}
	for _, id := range plan.remove {
		if err := sendJSON(ctx, http.MethodPost, b.URL+"/api/v1/models/model/delete", c.Key, map[string]string{"id": id}, nil); err != nil {
			return fmt.Errorf("removing workspace model '%s' from '%s': %w", id, b.Name, err)
		}
	}
	return nil
}
