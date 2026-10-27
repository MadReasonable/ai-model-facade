package testkit

import (
	"io"
	"net/http"
)

// Reply is how a fake llama-swap, llama-server, LM Studio or Ollama answers
// a completion request.
// The zero Reply answers 200 with DefaultBody.
type Reply struct {
	Status int               // default 200
	Header map[string]string // extra response headers, such as Retry-After
	Body   string            // JSON body; default DefaultBody

	// Stream, if not nil, answers with these server-sent event payloads
	// instead of Body, followed by [DONE] unless Truncate is set. ErrorEvent,
	// if set, is sent after them as an `event: error`, the way llama-server
	// reports a failure mid-stream.
	Stream     []string
	Truncate   bool
	ErrorEvent string
}

// DefaultBody is a minimal chat completion with a field the gateway doesn't
// know, so tests can check that responses pass through unchanged.
const DefaultBody = `{"choices":[{"message":{"content":"ok"}}],"x_extra":1}`

func (r Reply) write(w http.ResponseWriter) {
	for k, v := range r.Header {
		w.Header().Set(k, v)
	}
	if r.Stream != nil {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(r.status())
		for _, data := range r.Stream {
			io.WriteString(w, "data: "+data+"\n\n")
		}
		if r.ErrorEvent != "" {
			io.WriteString(w, "event: error\ndata: "+r.ErrorEvent+"\n\n")
		}
		if !r.Truncate && r.ErrorEvent == "" {
			io.WriteString(w, "data: [DONE]\n\n")
		}
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(r.status())
	body := r.Body
	if body == "" {
		body = DefaultBody
	}
	io.WriteString(w, body)
}

func (r Reply) status() int {
	if r.Status == 0 {
		return http.StatusOK
	}
	return r.Status
}
