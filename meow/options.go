/*
 * SPDX-FileCopyrightText: The uwuAOSP Project
 * SPDX-License-Identifier: Apache-2.0
 */

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

const giB int64 = 1 << 30

type options struct {
	args                 []string
	preview, plain, help bool
	memory               string
	gc                   string
}

const usage = `meow / mio — Android build dashboard

Usage: meow [dashboard options] [Soong arguments / targets...]
       mio  [dashboard options] [Soong arguments / targets...]

  --preview                 Preview the dashboard without building
  --plain                   Use ordinary output instead of the dashboard
  --analysis-memory=VALUE   auto (default), off, bytes, or e.g. 16GiB
  --analysis-gc=PERCENT     Soong analysis GC percentage (default 100)
  -h, --help                Show this help
  --                        Pass every remaining argument to Soong

Builds always use Runa, including Soong bootstrap. Executor settings
SOONG_NINJA, NO_ABFS and MEOW_NINJA_BIN cannot override this policy.
For the main build, -j sets initial admission; memory pressure lowers it,
and healthy memory plus idle CPU can raise it to 1.5 times that value.
Other arguments, including -j, -k, -l and NAME=value, go to
soong_ui.bash --make-mode unchanged. Run lunch before building.
Existing SOONG_ANALYSIS_MEMORY_LIMIT_BYTES / SOONG_ANALYSIS_GC_PERCENT
values are respected unless explicitly overridden above.

Examples:
  meow -j16 otapackage
  mio SystemUI
  meow --analysis-memory=18GiB -j12
  meow --preview
`

func parseOptions(args []string) (options, error) {
	o := options{}
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--":
			o.args = append(o.args, args[i+1:]...)
			return o, nil
		case a == "--preview":
			o.preview = true
		case a == "--plain":
			o.plain = true
		case a == "--help" || a == "-h":
			o.help = true
		case a == "--analysis-memory" || a == "--analysis-gc":
			if i+1 == len(args) || args[i+1] == "" {
				return o, fmt.Errorf("%s requires a value", a)
			}
			i++
			if a == "--analysis-memory" {
				o.memory = args[i]
			} else {
				o.gc = args[i]
			}
		case strings.HasPrefix(a, "--analysis-memory="):
			o.memory = strings.TrimPrefix(a, "--analysis-memory=")
			if o.memory == "" {
				return o, fmt.Errorf("--analysis-memory requires a value")
			}
		case strings.HasPrefix(a, "--analysis-gc="):
			o.gc = strings.TrimPrefix(a, "--analysis-gc=")
			if o.gc == "" {
				return o, fmt.Errorf("--analysis-gc requires a value")
			}
		default:
			if a == "showcommands" {
				o.plain = true
			}
			o.args = append(o.args, a)
		}
	}
	return o, nil
}

// Assignments are applied in command-line order, matching Soong's overrides.
func buildEnvironment(base, args []string) []string {
	env := append([]string(nil), base...)
	for _, arg := range args {
		key, value, ok := strings.Cut(arg, "=")
		if ok && key != "" && !strings.HasPrefix(key, "-") {
			env = setEnv(env, key, value)
		}
	}
	return env
}

func envValue(env []string, key string) (string, bool) {
	for i := len(env) - 1; i >= 0; i-- {
		if v, ok := strings.CutPrefix(env[i], key+"="); ok {
			return v, true
		}
	}
	return "", false
}

func setEnv(env []string, key, value string) []string {
	result := make([]string, 0, len(env)+1)
	for _, entry := range env {
		if !strings.HasPrefix(entry, key+"=") {
			result = append(result, entry)
		}
	}
	return append(result, key+"="+value)
}

func initialParallelism(args []string) int {
	jobs := runtime.NumCPU() + 2
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "-j" {
			jobs = runtime.NumCPU() + 2
			if i+1 < len(args) {
				if n, err := strconv.Atoi(args[i+1]); err == nil && n > 0 {
					jobs = n
					i++
				}
			}
		} else if strings.HasPrefix(arg, "-j") {
			if n, err := strconv.Atoi(strings.TrimPrefix(arg, "-j")); err == nil && n > 0 {
				jobs = n
			}
		}
	}
	return jobs
}

func sourceRoot(start string, env []string) (string, error) {
	for _, key := range []string{"TOP", "ANDROID_BUILD_TOP"} {
		if root, ok := envValue(env, key); ok && root != "" {
			if root, err := filepath.Abs(root); err == nil && isSourceRoot(root) {
				return root, nil
			}
		}
	}
	dir, err := filepath.Abs(start)
	if err != nil {
		return "", err
	}
	for {
		if isSourceRoot(dir) {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("run inside an Android source tree, after lunch")
		}
		dir = parent
	}
}

func isSourceRoot(root string) bool {
	info, err := os.Stat(filepath.Join(root, "build", "soong", "soong_ui.bash"))
	return err == nil && !info.IsDir()
}

func outputRoot(top string, env []string) string {
	out, _ := envValue(env, "OUT_DIR")
	if out == "" {
		if common, _ := envValue(env, "OUT_DIR_COMMON_BASE"); common != "" {
			out = filepath.Join(common, filepath.Base(top))
		} else {
			out = "out"
		}
	}
	if !filepath.IsAbs(out) {
		out = filepath.Join(top, out)
	}
	return filepath.Clean(out)
}

// This is a fresh, conservative policy, independent of execution concurrency.
// Go's limit is soft; retain both host-wide and currently available headroom.
func analysisBudget(total, available int64) int64 {
	if total <= 0 || available <= 0 {
		return 0
	}
	available = min(total, available)
	if available <= 2*giB {
		return available / 2
	}
	return min(total-total/4, available-available/5, available-2*giB)
}

func parseMemory(raw string) (int64, error) {
	if raw == "off" {
		return 0, nil
	}
	factor := int64(1)
	for _, unit := range []struct {
		name string
		size int64
	}{{"GiB", 1 << 30}, {"MiB", 1 << 20}, {"KiB", 1 << 10}} {
		if strings.HasSuffix(raw, unit.name) {
			raw = strings.TrimSuffix(raw, unit.name)
			factor = unit.size
			break
		}
	}
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || n < 0 || n > (1<<63-1)/factor {
		return 0, fmt.Errorf("invalid analysis memory budget %q", raw)
	}
	return n * factor, nil
}

func configureAnalysis(o options, env []string, total, available int64) ([]string, []string, error) {
	var overrides []string
	for _, item := range []struct{ key, explicit string }{
		{"SOONG_ANALYSIS_MEMORY_LIMIT_BYTES", o.memory}, {"SOONG_ANALYSIS_GC_PERCENT", o.gc},
	} {
		raw, exists := envValue(env, item.key)
		if item.explicit != "" {
			raw, exists = item.explicit, true
		}
		if item.key == "SOONG_ANALYSIS_MEMORY_LIMIT_BYTES" {
			if !exists || raw == "auto" {
				raw = strconv.FormatInt(analysisBudget(total, available), 10)
			}
			n, err := parseMemory(raw)
			if err != nil {
				return nil, nil, err
			}
			raw = strconv.FormatInt(n, 10)
		} else {
			if !exists {
				raw = "100"
			}
			if n, err := strconv.Atoi(raw); err != nil || n < 0 {
				return nil, nil, fmt.Errorf("invalid analysis GC percentage %q", raw)
			}
		}
		env = setEnv(env, item.key, raw)
		// Explicit dashboard options must also override NAME=value arguments.
		if item.explicit != "" {
			overrides = append(overrides, item.key+"="+raw)
		}
	}
	return env, overrides, nil
}
