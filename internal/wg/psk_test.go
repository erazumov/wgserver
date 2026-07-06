package wg

import (
	"errors"
	"strings"
	"testing"
)

type pskRunner struct {
	out string
	err error
}

func (p *pskRunner) Run(name string, args ...string) error { return p.err }

func (p *pskRunner) Output(name string, args ...string) (string, error) {
	if p.err != nil {
		return "", p.err
	}
	return p.out, nil
}

func TestGeneratePresharedKey_OK(t *testing.T) {
	r := &pskRunner{out: "PSK_BASE64=" + strings.Repeat("A", 43)}
	got, err := GeneratePresharedKey(r)
	if err != nil {
		t.Fatalf("GeneratePresharedKey: %v", err)
	}
	if !strings.HasPrefix(got, "PSK_BASE64=") {
		t.Errorf("got %q, want PSK_BASE64= prefix", got)
	}
}

func TestGeneratePresharedKey_EmptyErrors(t *testing.T) {
	r := &pskRunner{out: ""}
	_, err := GeneratePresharedKey(r)
	if !errors.Is(err, errEmptyPSK) {
		t.Errorf("err = %v, want errEmptyPSK", err)
	}
}

func TestGeneratePresharedKey_RunnerErrorWrapped(t *testing.T) {
	r := &pskRunner{err: errors.New("wg not found")}
	_, err := GeneratePresharedKey(r)
	if err == nil {
		t.Fatal("want error, got nil")
	}
	if !strings.Contains(err.Error(), "wg genpsk") {
		t.Errorf("err = %v, want it to mention 'wg genpsk'", err)
	}
}
