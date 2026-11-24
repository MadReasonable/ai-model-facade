package config

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"unicode"
)

// A name is what the config calls a facade, a backend, a key or a group, and
// the gateway prints names in the log, in check's output and in errors.
// checkName keeps every name printable and free of characters that change
// how the text around it reads: a terminal acts on a control character in
// check's output, and an invisible or bidi formatting character can make two
// different names read the same in an access list. The rule is the same for
// every script; only a facade name also takes '/', so a facade can be named
// after a model ID such as org/model.
//
// A name is defined once, where its entry is, and referred to elsewhere, in
// access lists, groups, key maps and candidates. Only definitions are
// checked here: a reference has to match a definition, and the error for one
// that doesn't quotes it with QuoteName, so a character it can't contain
// shows as an escape.

// maxNameLength is the most characters a name has, counted as code points.
const maxNameLength = 64

// zeroWidthNonJoiner is part of normal Persian spelling, between two letters
// that would otherwise join, so a name may contain it there; anywhere else
// it is invisible.
const zeroWidthNonJoiner = '\u200c'

// checkName fails if name isn't a valid name: a letter, then letters,
// combining marks and digits of any script and @ - _ . :, and / too if
// facade, at most maxNameLength of them. A letter comes first because a
// combining mark there draws on the character before the name, and a digit
// or punctuation there can move when a right-to-left name is displayed.
func checkName(name string, facade bool) error {
	if name == "" {
		return errors.New("a name can't be empty")
	}
	punctuation := "@-_.:"
	if facade {
		punctuation += "/"
	}
	runes := []rune(name)
	if !unicode.IsLetter(runes[0]) {
		return fmt.Errorf("%s starts with %s; a name starts with a letter", QuoteName(name), describeRune(runes[0]))
	}
	for i, r := range runes {
		switch {
		case unicode.IsLetter(r), unicode.IsMark(r), unicode.IsDigit(r), strings.ContainsRune(punctuation, r):
		case r == zeroWidthNonJoiner:
			if i+1 == len(runes) || !isArabicLetter(runes[i-1]) || !isArabicLetter(runes[i+1]) {
				return fmt.Errorf("%s contains U+200C, a zero-width non-joiner, which a name can contain only between two Arabic-script letters", QuoteName(name))
			}
		default:
			return fmt.Errorf("%s contains %s; a name contains only letters, combining marks, digits and %s", QuoteName(name), describeRune(r), strings.Join(strings.Split(punctuation, ""), " "))
		}
	}
	if len(runes) > maxNameLength {
		return fmt.Errorf("%s is %d characters long; a name is at most %d", QuoteName(name), len(runes), maxNameLength)
	}
	return nil
}

func isArabicLetter(r rune) bool {
	return unicode.IsLetter(r) && unicode.Is(unicode.Arabic, r)
}

// describeRune names r for an error: the character itself in single quotes
// if it is visible on its own, else its code point, which also shows a
// space, an invisible character or a combining mark that would draw on the
// quote before it.
func describeRune(r rune) string {
	if r != '\'' && unicode.In(r, unicode.L, unicode.N, unicode.P, unicode.S) {
		return "'" + string(r) + "'"
	}
	return fmt.Sprintf("U+%04X", r)
}

// QuoteName returns name in single quotes for a message, with each character
// that isn't visible on its own written as a Go escape such as \x1b or
// \u200b, so a name the config got wrong can't act on a terminal or hide
// what makes it differ from another. A combining mark first is escaped too,
// since it would draw on the opening quote. A valid name is returned
// unchanged inside the quotes.
func QuoteName(name string) string {
	var b strings.Builder
	b.WriteByte('\'')
	for i, r := range name {
		switch {
		case r == '\\' || r == '\'':
			b.WriteByte('\\')
			b.WriteRune(r)
		case i == 0 && unicode.IsMark(r):
			b.WriteString(strings.Trim(strconv.QuoteRuneToASCII(r), "'"))
		case r == ' ' || (unicode.IsGraphic(r) && !unicode.IsSpace(r)):
			b.WriteRune(r)
		default:
			b.WriteString(strings.Trim(strconv.QuoteRuneToASCII(r), "'"))
		}
	}
	b.WriteByte('\'')
	return b.String()
}
