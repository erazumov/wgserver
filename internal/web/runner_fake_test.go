package web

import (
	"io"
	"strings"

	"github.com/erazumov/wgserver/internal/wg"
)

// fakeRunner for handler tests. Stores the wg genkey/pubkey outputs so
// the test doesn't need a real `wg` binary in PATH.
type fakeRunner struct {
	genkey  string
	pubkey  string
	psk     string
	calls   []string
	failGen bool
	failPSK bool
}

func newFakeRunner() *fakeRunner {
	return &fakeRunner{
		genkey: "FAKE_PRIV_BASE64",
		pubkey: "FAKE_PUB_BASE64",
		psk:    "FAKE_PSK_BASE64=",
	}
}

func (f *fakeRunner) Run(name string, args ...string) error {
	f.calls = append(f.calls, name+" "+strings.Join(args, " "))
	return nil
}

func (f *fakeRunner) Output(name string, args ...string) (string, error) {
	f.calls = append(f.calls, name+" "+strings.Join(args, " "))
	if f.failGen {
		return "", nil
	}
	if f.failPSK && len(args) > 0 && args[0] == "genpsk" {
		return "", nil
	}
	switch name {
	case "wg":
		if len(args) > 0 {
			switch args[0] {
			case "genkey":
				return f.genkey, nil
			case "pubkey":
				// Production code path: wg pubkey now goes
				// through OutputStdin (we pipe the privkey via
				// stdin rather than passing the file as an
				// argument). But the fake delegates OutputStdin
				// to Output for simplicity, so this case must
				// still respond with a non-empty pubkey.
				return f.pubkey, nil
			case "genpsk":
				return f.psk, nil
			}
		}
	}
	return "", nil
}

// OutputStdin is the stdin-fed variant of Output. The production
// keys.go uses it for `wg pubkey` (which reads the privkey from
// stdin rather than an argument). The handler tests don't exercise
// the bot's keygen path, so this is a thin wrapper around Output
// — we just need it to satisfy the wg.Runner interface.
func (f *fakeRunner) OutputStdin(name string, args []string, _ io.Reader) (string, error) {
	return f.Output(name, args...)
}

var _ wg.Runner = (*fakeRunner)(nil)
