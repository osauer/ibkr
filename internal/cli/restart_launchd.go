package cli

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"strconv"
	"strings"
)

// appLaunchAgentLabel matches the LaunchAgent installed by `ibkr setup app`.
const appLaunchAgentLabel = "com.osauer.ibkr-app"

// appSupervisor describes a loaded launchd job that owns the app process.
type appSupervisor struct {
	Target     string   // launchctl target, e.g. gui/501/com.osauer.ibkr-app
	PID        int      // supervised pid, 0 while launchd has no live process
	Executable string   // leading plist ProgramArguments executable
	Args       []string // plist ProgramArguments starting at "app"
}

var launchdPIDRe = regexp.MustCompile(`(?m)^\s*pid = (\d+)\b`)

// findAppLaunchAgent reports the loaded app LaunchAgent, if any. A restart
// must go through launchd when it owns the app: SIGTERM-plus-respawn races
// launchd's KeepAlive and strands an orphan that holds the app state lock
// while launchd crash-loops against it.
func findAppLaunchAgent(ctx context.Context) (appSupervisor, bool) {
	if runtime.GOOS != "darwin" {
		return appSupervisor{}, false
	}
	target := fmt.Sprintf("gui/%d/%s", os.Getuid(), appLaunchAgentLabel)
	out, err := exec.CommandContext(ctx, "launchctl", "print", target).Output()
	if err != nil {
		return appSupervisor{}, false
	}
	sup := appSupervisor{Target: target}
	if m := launchdPIDRe.FindStringSubmatch(string(out)); m != nil {
		if pid, err := strconv.Atoi(m[1]); err == nil {
			sup.PID = pid
		}
	}
	sup.Executable, sup.Args = launchdProgramArguments(string(out))
	return sup, true
}

// launchdProgramArguments extracts both the leading executable and the app
// args ("app", "--remote", ...) from `launchctl print` output.
func launchdProgramArguments(out string) (string, []string) {
	var args []string
	inBlock := false
	for line := range strings.SplitSeq(out, "\n") {
		trimmed := strings.TrimSpace(line)
		if !inBlock {
			if trimmed == "arguments = {" {
				inBlock = true
			}
			continue
		}
		if trimmed == "}" {
			break
		}
		args = append(args, trimmed)
	}
	if len(args) == 0 {
		return "", nil
	}
	for i, arg := range args[1:] {
		if arg == "app" {
			return args[0], args[i+1:]
		}
	}
	return args[0], nil
}

func kickstartLaunchAgent(ctx context.Context, target string) error {
	out, err := exec.CommandContext(ctx, "launchctl", "kickstart", "-k", target).CombinedOutput()
	if err != nil {
		return fmt.Errorf("launchctl kickstart -k %s: %v: %s", target, err, strings.TrimSpace(string(out)))
	}
	return nil
}
