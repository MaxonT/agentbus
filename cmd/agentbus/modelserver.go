package main

// A list of a subscription's models from the harness's own model server
// (harness.ModelServer: codex app-server and its model/list). Control runs
// the unmodified harness the way a runner that holds no secret runs a turn
// of it: in a clean home of its own, made for the one listing and removed
// after, with HOME and the harness's config variable there and none of this
// user's home variables, wired as a relayed turn (WireModels). Its one call
// for the list goes to control's relay on this service with a listing token
// as its placeholder (jobs.Server.ModelsToken); the relay adds the
// subscription kept on control to that call only and records it. The run is
// bounded, in its own process group, and stopped by that group once the
// list is read. Nothing it prints is kept but the models' names and labels.

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/staysup/agentbus/harness"
	"github.com/staysup/agentbus/jobs"
	"github.com/staysup/agentbus/models"
)

// modelHomesDir holds the homes of listings of models running now, in the
// data folder.
const modelHomesDir = "model-homes"

// modelServerWait bounds one listing.
const modelServerWait = time.Minute

// homeEnv are the variables that point a program at a folder of this
// user's home: a listing goes without them (as run's homeVars).
var homeEnv = []string{"HOME", "XDG_CONFIG_HOME", "XDG_DATA_HOME", "XDG_STATE_HOME", "XDG_CACHE_HOME"}

// modelServer lists a harness's models from its own model server, its call
// through the relay of the service at baseURL. Errors are fixed wording.
func modelServer(c config, js *jobs.Server, baseURL string) func(ctx context.Context, name string) ([]models.Model, error) {
	return func(ctx context.Context, name string) ([]models.Model, error) {
		h, ok := harness.Get(name)
		if !ok {
			return nil, models.ErrServer
		}
		ms, ok := h.(harness.ModelServer)
		if !ok {
			return nil, models.ErrServer
		}
		info := h.Info()
		bin, err := c.lookPath(name)
		if err != nil {
			return nil, errors.New(info.Label + " is not installed where control runs, so its models cannot be listed; install " + info.Product)
		}
		base, token, end, ok := js.ModelsToken(name)
		if !ok {
			return nil, models.ErrServer
		}
		defer end()
		dir := filepath.Join(c.home, modelHomesDir)
		if os.MkdirAll(dir, 0o700) != nil {
			return nil, models.ErrServer
		}
		home, err := os.MkdirTemp(dir, name+"-")
		if err != nil {
			return nil, models.ErrServer
		}
		defer os.RemoveAll(home)
		set := []string{"HOME=" + home}
		if info.ConfigEnv != "" {
			conf := filepath.Join(home, info.ConfigDir)
			if os.MkdirAll(conf, 0o700) != nil {
				return nil, models.ErrServer
			}
			set = append(set, info.ConfigEnv+"="+conf)
		}
		env, args := ms.WireModels(baseURL+base, token)
		parent := slices.DeleteFunc(c.environ(), func(kv string) bool {
			k, _, _ := strings.Cut(kv, "=")
			return slices.Contains(homeEnv, k)
		})
		ctx, cancel := context.WithTimeout(ctx, modelServerWait)
		defer cancel()
		cmd := exec.Command(bin, args...)
		cmd.Env = childEnv(parent, append(set, env...))
		cmd.Dir = home
		// Its own process group, so stopping it stops whatever it started.
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		in, err := cmd.StdinPipe()
		if err != nil {
			return nil, models.ErrServer
		}
		out, err := cmd.StdoutPipe()
		if err != nil {
			return nil, models.ErrServer
		}
		if cmd.Start() != nil {
			return nil, models.ErrServer
		}
		defer func() {
			_ = in.Close()
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			_ = cmd.Wait()
		}()
		return models.AppServer(ctx, in, out, "agentbus", version)
	}
}
