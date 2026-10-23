package nodocker

import (
	"errors"
	"os"
	"os/exec"
	"reflect"
	"testing"
)

func TestInstall(t *testing.T) {
	t.Setenv("PATH", os.Getenv("PATH"))
	t.Setenv("DOCKER_HOST", "unix:///nowhere.sock")
	t.Setenv("DOCKER_CONTEXT", "elsewhere")
	recorded, err := Install(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if got := recorded(); got != nil {
		t.Errorf("recorded %q before any call", got)
	}
	for _, k := range []string{"DOCKER_HOST", "DOCKER_CONTEXT"} {
		if v, ok := os.LookupEnv(k); ok {
			t.Errorf("%s=%q, want unset", k, v)
		}
	}
	err = exec.Command("docker", "info").Run()
	var ee *exec.ExitError
	if !errors.As(err, &ee) || ee.ExitCode() != 125 {
		t.Errorf("docker info: %v, want exit 125", err)
	}
	// A command given an environment of its own still finds the stub on
	// the PATH it was given, and is still recorded.
	p, err := exec.LookPath("docker")
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(p, "ps", "-a")
	cmd.Env = []string{"PATH=/usr/bin:/bin"}
	_ = cmd.Run()
	if got, want := recorded(), []string{"docker info", "docker ps -a"}; !reflect.DeepEqual(got, want) {
		t.Errorf("recorded %q, want %q", got, want)
	}
}
