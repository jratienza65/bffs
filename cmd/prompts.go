package cmd

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"golang.org/x/term"
)

// prompter reads a sequence of prompts from one stream.
//
// It exists because bufio.Reader reads ahead: constructing a fresh one per
// prompt lets the first read swallow the rest of the input, so a second
// prompt sees EOF. That is invisible with a terminal (input arrives a line at
// a time) and breaks immediately with piped input. Any flow that prompts more
// than once must share a single prompter.
type prompter struct {
	r   *bufio.Reader
	out io.Writer
}

func newPrompter(in io.Reader, out io.Writer) *prompter {
	return &prompter{r: bufio.NewReader(in), out: out}
}

// line writes prompt and reads one line of input.
//
// Exhausted input is reported as an empty answer rather than an error: every
// caller has a safe default for "no answer" (keep the default, or decline),
// and surfacing a bare "EOF" to the user explains nothing.
func (p *prompter) line(prompt string) (string, error) {
	fmt.Fprint(p.out, prompt)
	line, err := p.r.ReadString('\n')
	if err != nil {
		if errors.Is(err, io.EOF) {
			fmt.Fprintln(p.out)
			return strings.TrimRight(line, "\r\n"), nil
		}
		if line == "" {
			return "", err
		}
	}
	return strings.TrimRight(line, "\r\n"), nil
}

// promptLine reads a single line. It consumes buffered input, so it is safe
// only for one-shot prompts — use a shared prompter when asking more than
// one question from the same stream.
func promptLine(in io.Reader, out io.Writer, prompt string) (string, error) {
	fmt.Fprint(out, prompt)
	r := bufio.NewReader(in)
	line, err := r.ReadString('\n')
	if err != nil && line == "" {
		return "", err
	}
	return strings.TrimRight(line, "\r\n"), nil
}

// defaultNameFromEmail derives an account name from the local part of an
// email (e.g. "team@example.com" → "team"). Returns "" if email is empty.
func defaultNameFromEmail(email string) string {
	if email == "" {
		return ""
	}
	if i := strings.IndexByte(email, '@'); i > 0 {
		return email[:i]
	}
	return email
}

// promptSecret reads a secret with terminal echo off when stdin is a TTY,
// and falls back to a regular line read otherwise (so tests / pipes still work).
func promptSecret(out io.Writer, prompt string) (string, error) {
	fmt.Fprint(out, prompt)
	fd := int(os.Stdin.Fd())
	if term.IsTerminal(fd) {
		b, err := term.ReadPassword(fd)
		fmt.Fprintln(out)
		if err != nil {
			return "", err
		}
		return strings.TrimRight(string(b), "\r\n"), nil
	}
	return promptLine(os.Stdin, out, "")
}
