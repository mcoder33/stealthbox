package stealthbox

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

func TestQuote(t *testing.T) {
	for _, s := range []string{"hello", "a'b", "$(touch /tmp/nope)", "a\nb", ""} {
		out, err := exec.Command("sh", "-c", "printf %s "+Quote(s)).Output()
		if err != nil || string(out) != s {
			t.Fatalf("quote %q: %q %v", s, out, err)
		}
	}
}
func project() Project {
	return Project{Source: Endpoint{Host: "vm", Path: "/work/project"}, Runners: map[string]Endpoint{"vm": {Host: "vm", Path: "/work/project"}, "mac": {Path: "/tmp/runner"}}}
}
func TestRunSameHost(t *testing.T) {
	c, e := PlanRun(project(), "vm", []string{"echo", "a'b"})
	if e != nil || len(c) != 1 {
		t.Fatalf("%v %v", c, e)
	}
	if c[0].Program != "ssh" {
		t.Fatal(c)
	}
}
func TestSync(t *testing.T) {
	c, e := PlanRun(project(), "mac", []string{"docker", "compose", "run", "--rm", "test"})
	if e != nil || len(c) != 3 {
		t.Fatalf("%v %v", c, e)
	}
	a := strings.Join(c[1].Args, " ")
	if !strings.Contains(a, "--delete-delay") || !strings.Contains(a, "--exclude=.env") || !strings.Contains(a, "vm:/work/project/") {
		t.Fatal(a)
	}
}
func TestRejectUnsafe(t *testing.T) {
	for _, e := range []Endpoint{{Path: "/"}, {Path: "relative"}, {Host: "-oProxyCommand=bad", Path: "/work"}} {
		if ValidateEndpoint(e) == nil {
			t.Fatal(e)
		}
	}
	p := project()
	p.Runners["other"] = Endpoint{Host: "other", Path: "/work"}
	if _, e := PlanRun(p, "other", []string{"true"}); e == nil {
		t.Fatal("remote-to-remote accepted")
	}
}
func TestShellCDMustSucceed(t *testing.T) {
	p := project()
	p.Source = Endpoint{Path: "/definitely-missing-stealthbox-path"}
	c, e := Open(p, "example", "shell", "vm", []string{"touch", t.TempDir() + "/should-not-exist"})
	if e != nil {
		t.Fatal(e)
	}
	if exec.Command(c.Program, c.Args...).Run() == nil {
		t.Fatal("missing directory accepted")
	}
}
func TestOpen(t *testing.T) {
	c, e := Open(project(), "example", "tmux", "mac", []string{"codex"})
	if e != nil {
		t.Fatal(e)
	}
	s := strings.Join(c.Args, " ")
	if !strings.Contains(s, "stealthbox-example-mac") || !strings.Contains(s, "STEALTHBOX_RUNNER") {
		t.Fatal(s)
	}
}
func TestRunnerGuard(t *testing.T) {
	p := project()
	p.Source = Endpoint{Host: "vm", Path: "/work"}
	p.Runners["mac"] = Endpoint{Path: t.TempDir()}
	cmds, e := PlanRun(p, "mac", []string{"true"})
	if e != nil {
		t.Fatal(e)
	}
	if out, e := exec.Command(cmds[0].Program, cmds[0].Args...).CombinedOutput(); e != nil {
		t.Fatalf("%s %v", out, e)
	}
}

func TestGuardRejectsNonempty(t *testing.T) {
	p := project()
	p.Runners["mac"] = Endpoint{Path: t.TempDir()}
	if err := os.WriteFile(p.Runners["mac"].Path+"/valuable", []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	c, e := PlanRun(p, "mac", []string{"true"})
	if e != nil {
		t.Fatal(e)
	}
	if exec.Command(c[0].Program, c[0].Args...).Run() == nil {
		t.Fatal("unmarked nonempty directory accepted")
	}
}
func TestRejectOverlappingDirectories(t *testing.T) {
	for _, dst := range []string{"/work/project/runner", "/work"} {
		p := project()
		p.Source.Host = ""
		p.Runners["mac"] = Endpoint{Path: dst}
		if _, e := PlanRun(p, "mac", []string{"true"}); e == nil {
			t.Fatalf("overlap accepted: %s", dst)
		}
	}
}
