package cmd

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strings"

	"golang.org/x/term"
)

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
