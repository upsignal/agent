package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestReadTokenFromPipe(t *testing.T) {
	const token = "AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8" // 32 bytes, base64url
	for _, in := range []string{token, token + "\n", "  " + token + "\r\n", token + "\nignored"} {
		got, err := readToken(strings.NewReader(in), &bytes.Buffer{})
		if err != nil || got != token {
			t.Errorf("readToken(%q) = %q, %v", in, got, err)
		}
	}
	for _, in := range []string{"", "short", token + "=", strings.Repeat("A", 44), "!" + token[1:]} {
		var stderr bytes.Buffer
		if _, err := readToken(strings.NewReader(in), &stderr); err == nil {
			t.Errorf("readToken(%q) accepted an invalid token", in)
		} else if strings.Contains(err.Error(), in) && in != "" {
			t.Errorf("error echoes the input: %v", err)
		}
	}
}

func TestUnknownCommandAndArgs(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := run([]string{"bogus"}, strings.NewReader(""), &out, &errOut); code != exitUsage {
		t.Errorf("unknown command exit = %d", code)
	}
	if code := run([]string{"enroll", "AAAA"}, strings.NewReader(""), &out, &errOut); code != exitUsage {
		t.Errorf("token as argument exit = %d; tokens must never be accepted on the command line", code)
	}
}
