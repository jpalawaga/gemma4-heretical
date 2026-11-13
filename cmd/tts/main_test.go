package main

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestParseConfig(t *testing.T) {
	cfg, err := parseConfig([]string{
		"--voice", "Aiden",
		"--language", "English",
		"--instruct", "Sound quietly delighted.",
		"--speed", "1.25",
		"--output-dir", t.TempDir(),
		"--timeout", "2m",
		"--no-play",
	})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Mode != "design" || cfg.DesignModel != defaultDesignModel ||
		cfg.PresetModel != defaultPresetModel || cfg.Voice != "Aiden" || cfg.Language != "English" ||
		cfg.Instruct != "Sound quietly delighted." || cfg.Speed != 1.25 {
		t.Fatalf("unexpected config: %#v", cfg)
	}
	if cfg.Timeout != 2*time.Minute || !cfg.DisablePlay || !filepath.IsAbs(cfg.OutputDir) {
		t.Fatalf("config validation was not applied: %#v", cfg)
	}
}

func TestParseConfigRejectsInvalidSpeed(t *testing.T) {
	if _, err := parseConfig([]string{"--speed", "2.1"}); err == nil {
		t.Fatal("accepted an out-of-range speed")
	}
}

func TestParseConfigRejectsInvalidMode(t *testing.T) {
	if _, err := parseConfig([]string{"--mode", "clone"}); err == nil {
		t.Fatal("accepted an unsupported voice mode")
	}
}

func TestTextAreaStartsFocused(t *testing.T) {
	cfg := config{Mode: "design", Voice: "Ryan", Language: "English", Speed: 1}
	m := newModel(cfg, backend{Path: "fake"})
	if !m.composer.Focused() {
		t.Fatal("text area did not start focused")
	}
	if m.direction.Focused() {
		t.Fatal("performance direction unexpectedly started focused")
	}
}

func TestFocusCurrentMovesBetweenEditors(t *testing.T) {
	cfg := config{Mode: "design", Voice: "Ryan", Language: "English", Speed: 1}
	m := newModel(cfg, backend{Path: "fake"})
	m.focus = 1
	_ = m.focusCurrent()
	if m.composer.Focused() || !m.design.Focused() || m.direction.Focused() {
		t.Fatal("focus did not move to voice design")
	}
	m.focus = 2
	_ = m.focusCurrent()
	if m.composer.Focused() || m.design.Focused() || !m.direction.Focused() {
		t.Fatal("focus did not move to performance direction")
	}
	m.focus = 0
	_ = m.focusCurrent()
	if !m.composer.Focused() || m.direction.Focused() {
		t.Fatal("focus did not return to script")
	}
}

func TestMoveFocusUsesFieldsForCurrentMode(t *testing.T) {
	m := newModel(config{Mode: "design", Voice: "Ryan"}, backend{Path: "fake"})
	m.moveFocus(1)
	if m.focus != 1 {
		t.Fatalf("design mode first Tab moved to field %d", m.focus)
	}
	m.cfg.Mode = "preset"
	m.focus = 0
	m.moveFocus(1)
	if m.focus != 2 {
		t.Fatalf("preset mode first Tab moved to field %d", m.focus)
	}
}

func TestDesignInstructionSeparatesIdentityAndPerformance(t *testing.T) {
	got := designInstruction(
		"a weathered alto with a soft Newfoundland accent",
		"hold back tears without becoming melodramatic",
	)
	want := "Voice identity: a weathered alto with a soft Newfoundland accent\n" +
		"Performance for this line: hold back tears without becoming melodramatic"
	if got != want {
		t.Fatalf("designInstruction() = %q, want %q", got, want)
	}
	if got := designInstruction("a clear tenor", ""); got != "a clear tenor" {
		t.Fatalf("identity-only designInstruction() = %q", got)
	}
}

func TestSynthesizeBuildsSafeArgumentsAndFindsOutput(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell fixture is POSIX-only")
	}
	dir := t.TempDir()
	argsFile := filepath.Join(dir, "args.txt")
	backendPath := filepath.Join(dir, "fake-tts")
	script := `#!/bin/sh
set -eu
: > "$ARGS_FILE"
output=
prefix=
while [ "$#" -gt 0 ]; do
  printf '%s\n' "$1" >> "$ARGS_FILE"
  case "$1" in
    --output_path) shift; output=$1; printf '%s\n' "$1" >> "$ARGS_FILE" ;;
    --file_prefix) shift; prefix=$1; printf '%s\n' "$1" >> "$ARGS_FILE" ;;
  esac
  shift
done
: > "$output/$prefix.wav"
`
	if err := os.WriteFile(backendPath, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ARGS_FILE", argsFile)
	cfg := config{
		Mode:        "preset",
		PresetModel: defaultPresetModel,
		Language:    "English",
		OutputDir:   dir,
		Timeout:     time.Minute,
	}
	path, _, err := synthesize(
		context.Background(),
		cfg,
		backend{Path: backendPath, Prefix: []string{"fixed-prefix"}},
		"hello; this stays one argument",
		"Ryan",
		"Start cautiously, then become confident.",
		1.15,
	)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Ext(path) != ".wav" {
		t.Fatalf("output path = %q", path)
	}
	rawArgs, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatal(err)
	}
	args := strings.Split(strings.TrimSpace(string(rawArgs)), "\n")
	for _, expected := range []string{
		"fixed-prefix",
		"--model", defaultPresetModel,
		"--text", "hello; this stays one argument",
		"--voice", "Ryan",
		"--lang_code", "English",
		"--speed", "1.15",
		"--instruct", "Start cautiously, then become confident.",
		"--join_audio",
	} {
		if !slices.Contains(args, expected) {
			t.Fatalf("arguments omitted %q: %#v", expected, args)
		}
	}
}

func TestSynthesizeOmitsEmptyInstruction(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell fixture is POSIX-only")
	}
	dir := t.TempDir()
	argsFile := filepath.Join(dir, "args.txt")
	backendPath := filepath.Join(dir, "fake-tts")
	script := `#!/bin/sh
set -eu
: > "$ARGS_FILE"
output=
prefix=
while [ "$#" -gt 0 ]; do
  printf '%s\n' "$1" >> "$ARGS_FILE"
  case "$1" in
    --output_path) shift; output=$1; printf '%s\n' "$1" >> "$ARGS_FILE" ;;
    --file_prefix) shift; prefix=$1; printf '%s\n' "$1" >> "$ARGS_FILE" ;;
  esac
  shift
done
: > "$output/$prefix.wav"
`
	if err := os.WriteFile(backendPath, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ARGS_FILE", argsFile)
	cfg := config{
		Mode:        "design",
		DesignModel: defaultDesignModel,
		Language:    "English",
		OutputDir:   dir,
		Timeout:     time.Minute,
	}
	if _, _, err := synthesize(
		context.Background(),
		cfg,
		backend{Path: backendPath},
		"hello",
		"",
		"",
		1,
	); err != nil {
		t.Fatal(err)
	}
	rawArgs, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatal(err)
	}
	args := strings.Split(strings.TrimSpace(string(rawArgs)), "\n")
	if slices.Contains(args, "--instruct") {
		t.Fatal("empty performance instruction was sent to the backend")
	}
	if slices.Contains(args, "--voice") {
		t.Fatal("voice argument was sent in VoiceDesign mode")
	}
}

func TestNewestMatchingWAV(t *testing.T) {
	dir := t.TempDir()
	older := filepath.Join(dir, "speech-test-000.wav")
	newer := filepath.Join(dir, "speech-test.wav")
	if err := os.WriteFile(older, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	oldTime := time.Now().Add(-time.Minute)
	if err := os.Chtimes(older, oldTime, oldTime); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(newer, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := newestMatchingWAV(dir, "speech-test")
	if err != nil {
		t.Fatal(err)
	}
	if got != newer {
		t.Fatalf("newest output = %q, want %q", got, newer)
	}
}
