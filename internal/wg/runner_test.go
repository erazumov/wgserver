package wg

import (
	"errors"
	"os/exec"
	"testing"
)

type fakeCall struct {
	Name string
	Args []string
}

type fakeRunner struct {
	calls       []fakeCall
	runErr      error
	outputs     map[string]string
	outputCalls []fakeCall
}

func (f *fakeRunner) Run(name string, args ...string) error {
	cp := make([]string, len(args))
	copy(cp, args)
	f.calls = append(f.calls, fakeCall{Name: name, Args: cp})
	return f.runErr
}

func (f *fakeRunner) Output(name string, args ...string) (string, error) {
	cp := make([]string, len(args))
	copy(cp, args)
	f.outputCalls = append(f.outputCalls, fakeCall{Name: name, Args: cp})
	keys := []string{
		outputKey(name, cp),
		name,
		name + " " + args[0],
	}
	if len(args) == 0 {
		keys = []string{name}
	}
	for _, k := range keys {
		if out, ok := f.outputs[k]; ok {
			return out, nil
		}
	}
	return "", errors.New("fakeRunner: no output configured for " + outputKey(name, cp))
}

func outputKey(name string, args []string) string {
	if len(args) == 0 {
		return name
	}
	return name + " " + joinArgs(args)
}

func joinArgs(a []string) string {
	out := ""
	for i, s := range a {
		if i > 0 {
			out += " "
		}
		out += s
	}
	return out
}

func TestFakeRunner_RecordsRunCalls(t *testing.T) {
	r := &fakeRunner{}
	if err := r.Run("wg", "set", "wg1"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(r.calls) != 1 {
		t.Fatalf("calls = %d, want 1", len(r.calls))
	}
	if r.calls[0].Name != "wg" || r.calls[0].Args[0] != "set" || r.calls[0].Args[1] != "wg1" {
		t.Errorf("call = %+v", r.calls[0])
	}
}

func TestFakeRunner_RecordsOutputCalls(t *testing.T) {
	r := &fakeRunner{outputs: map[string]string{"wg genkey": "PRIVKEY_OUTPUT\n"}}
	out, err := r.Output("wg", "genkey")
	if err != nil {
		t.Fatalf("Output: %v", err)
	}
	if out != "PRIVKEY_OUTPUT\n" {
		t.Errorf("out = %q, want PRIVKEY_OUTPUT\\n", out)
	}
	if len(r.outputCalls) != 1 || r.outputCalls[0].Name != "wg" {
		t.Errorf("outputCalls = %+v", r.outputCalls)
	}
}

func TestFakeRunner_RunReturnsConfiguredError(t *testing.T) {
	r := &fakeRunner{runErr: errors.New("boom")}
	err := r.Run("wg", "set", "wg1")
	if err == nil || err.Error() != "boom" {
		t.Errorf("err = %v, want boom", err)
	}
}

func TestExecRunner_Run_PropagatesExitError(t *testing.T) {
	r := &ExecRunner{}
	err := r.Run("/bin/sh", "-c", "exit 7")
	if err == nil {
		t.Fatal("Run /bin/sh exit 7: want error, got nil")
	}
	var ee *exec.ExitError
	if !errors.As(err, &ee) {
		t.Fatalf("err = %T, want *exec.ExitError", err)
	}
	if ee.ExitCode() != 7 {
		t.Errorf("ExitCode = %d, want 7", ee.ExitCode())
	}
}

func TestExecRunner_Output_TrimsTrailingNewline(t *testing.T) {
	r := &ExecRunner{}
	out, err := r.Output("/bin/sh", "-c", "echo hello")
	if err != nil {
		t.Fatalf("Output: %v", err)
	}
	if out != "hello" {
		t.Errorf("out = %q, want hello", out)
	}
}

func TestExecRunner_Output_MissingBinary(t *testing.T) {
	r := &ExecRunner{}
	_, err := r.Output("/nonexistent/binary/that/should/not/exist")
	if err == nil {
		t.Fatal("Output missing: want error, got nil")
	}
}
