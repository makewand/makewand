package router

import (
	"fmt"
	"os"
	"os/exec"
	"sync"
	"testing"
)

func TestJSONUserStoreConcurrentObjects(t *testing.T) {
	dir := t.TempDir()
	var wg sync.WaitGroup
	start := make(chan struct{})
	errs := make(chan error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			_, err := NewUserStore(dir).CreateUser(fmt.Sprintf("user%d@example.invalid", i), "test-password")
			errs <- err
		}(i)
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	users, err := NewUserStore(dir).ListUsers()
	if err != nil {
		t.Fatal(err)
	}
	if len(users) != 2 {
		t.Fatalf("successful users lost: %d", len(users))
	}
}

func TestJSONUserStoreProcessHelper(t *testing.T) {
	dir := os.Getenv("MW_JSON_TEST_DIR")
	if dir == "" {
		t.Skip("subprocess helper")
	}
	if _, err := NewUserStore(dir).CreateUser(os.Getenv("MW_JSON_TEST_EMAIL"), "test-password"); err != nil {
		t.Fatal(err)
	}
}

func TestJSONUserStoreConcurrentProcesses(t *testing.T) {
	dir := t.TempDir()
	commands := make([]*exec.Cmd, 2)
	for i := range commands {
		// #nosec G204 G702 -- re-execute this test binary with a literal helper test selector; no external executable or user argument.
		commands[i] = exec.Command(os.Args[0], "-test.run=^TestJSONUserStoreProcessHelper$")
		commands[i].Env = append(os.Environ(), "MW_JSON_TEST_DIR="+dir, fmt.Sprintf("MW_JSON_TEST_EMAIL=process%d@example.invalid", i))
		if err := commands[i].Start(); err != nil {
			t.Fatal(err)
		}
	}
	for _, cmd := range commands {
		if err := cmd.Wait(); err != nil {
			t.Fatal(err)
		}
	}
	users, err := NewUserStore(dir).ListUsers()
	if err != nil {
		t.Fatal(err)
	}
	if len(users) != 2 {
		t.Fatalf("successful subprocess users lost: %d", len(users))
	}
}
