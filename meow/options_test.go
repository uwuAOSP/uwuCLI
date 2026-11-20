/*
 * SPDX-FileCopyrightText: The uwuAOSP Project
 * SPDX-License-Identifier: Apache-2.0
 */

package main

import (
	"reflect"
	"testing"
)

func TestNativeArgumentsArePreserved(t *testing.T) {
	args := []string{"-j", "12", "-k0", "-l8", "SystemUI", "NAME=a b", "--", "--preview"}
	o, err := parseOptions(args)
	if err != nil {
		t.Fatal(err)
	}
	want := append(append([]string(nil), args[:6]...), "--preview")
	if !reflect.DeepEqual(o.args, want) || o.preview {
		t.Fatalf("options=%+v, want args=%q", o, want)
	}
}

func TestDashboardOptions(t *testing.T) {
	o, err := parseOptions([]string{"--analysis-memory=16GiB", "--analysis-gc", "75", "--plain", "-j8", "otapackage"})
	if err != nil || o.memory != "16GiB" || o.gc != "75" || !o.plain || !reflect.DeepEqual(o.args, []string{"-j8", "otapackage"}) {
		t.Fatalf("options=%+v err=%v", o, err)
	}
	if _, err := parseOptions([]string{"--analysis-gc"}); err == nil {
		t.Fatal("missing value accepted")
	}
}

func TestAnalysisPolicyAndOverrides(t *testing.T) {
	for _, tt := range []struct{ total, available, want int64 }{{0, 0, 0}, {16 * giB, 16 * giB, 12 * giB}, {32 * giB, 10 * giB, 8 * giB}, {32 * giB, giB, giB / 2}, {8 * giB, 16 * giB, 6 * giB}} {
		if got := analysisBudget(tt.total, tt.available); got != tt.want {
			t.Errorf("budget(%d,%d)=%d want %d", tt.total, tt.available, got, tt.want)
		}
	}
	env := buildEnvironment([]string{"SOONG_ANALYSIS_MEMORY_LIMIT_BYTES=1"}, []string{"SOONG_ANALYSIS_MEMORY_LIMIT_BYTES=2"})
	preserved, overrides, err := configureAnalysis(options{}, env, 32*giB, 30*giB)
	if err != nil || len(overrides) != 0 {
		t.Fatalf("overrides=%v err=%v", overrides, err)
	}
	if v, _ := envValue(preserved, "SOONG_ANALYSIS_MEMORY_LIMIT_BYTES"); v != "2" {
		t.Fatal(preserved)
	}
	explicit, overrides, err := configureAnalysis(options{memory: "16GiB", gc: "75"}, env, 32*giB, 30*giB)
	if err != nil {
		t.Fatal(err)
	}
	if v, _ := envValue(explicit, "SOONG_ANALYSIS_MEMORY_LIMIT_BYTES"); v != "17179869184" {
		t.Fatal(explicit)
	}
	if len(overrides) != 2 {
		t.Fatalf("explicit settings must override native assignments: %v", overrides)
	}
}

func TestMemoryValues(t *testing.T) {
	for _, raw := range []string{"-1", "999999999999GiB", "1.5GiB", "garbage"} {
		if _, err := parseMemory(raw); err == nil {
			t.Errorf("accepted %q", raw)
		}
	}
	if n, err := parseMemory("off"); err != nil || n != 0 {
		t.Fatalf("off=%d %v", n, err)
	}
	if _, _, err := configureAnalysis(options{gc: "-1"}, nil, 0, 0); err == nil {
		t.Fatal("negative GC accepted")
	}
}

func TestOutputDirectory(t *testing.T) {
	if got := outputRoot("/src/tree", []string{"OUT_DIR=custom"}); got != "/src/tree/custom" {
		t.Fatal(got)
	}
	if got := outputRoot("/src/tree", []string{"OUT_DIR_COMMON_BASE=/outputs"}); got != "/outputs/tree" {
		t.Fatal(got)
	}
}
