package provider

import (
	"reflect"
	"strings"
	"testing"
)

func TestValidateJournaldName(t *testing.T) {
	for _, name := range []string{"90-hardening", "retention", "a_b+c@d.e"} {
		if err := validateJournaldName(name); err != nil {
			t.Errorf("validateJournaldName(%q) = %v", name, err)
		}
	}
	for name, want := range map[string]string{
		"":                       "empty",
		"x.conf":                 `".conf"`,
		".hidden":                "must not start",
		"-x":                     "must not start",
		"a/b":                    "must consist",
		"a b":                    "must consist",
		strings.Repeat("a", 251): "at most 250",
	} {
		err := validateJournaldName(name)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("validateJournaldName(%q) = %v, want an error containing %q", name, err, want)
		}
	}
}

func TestValidateJournaldValues(t *testing.T) {
	for _, s := range []string{"500M", "1G", "1.5G", "4096", "8E"} {
		if err := validateJournaldSize(s); err != nil {
			t.Errorf("validateJournaldSize(%q) = %v", s, err)
		}
	}
	for _, s := range []string{"", "500MB", "1g", "-1", "1 G", "lots"} {
		if validateJournaldSize(s) == nil {
			t.Errorf("validateJournaldSize(%q) = nil, want an error", s)
		}
	}
	for _, s := range []string{"0", "1month", "2weeks", "2 weeks", "36h", "1d 12h", "1.5d", "90", "1y", "infinity", "10min 30s", "5µs"} {
		if err := validateJournaldTimespan(s); err != nil {
			t.Errorf("validateJournaldTimespan(%q) = %v", s, err)
		}
	}
	for _, s := range []string{"", " 1d", "1d ", "1 fortnight", "d", "-1d", "1d;2h"} {
		if validateJournaldTimespan(s) == nil {
			t.Errorf("validateJournaldTimespan(%q) = nil, want an error", s)
		}
	}
	for _, k := range []string{"RateLimitBurst", "MaxFileSec", "Audit"} {
		if err := validateJournaldKey(k); err != nil {
			t.Errorf("validateJournaldKey(%q) = %v", k, err)
		}
	}
	for _, k := range []string{"", "rateLimitBurst", "Rate Limit", "Rate=Limit", "Rate-Limit", "[Journal]"} {
		if validateJournaldKey(k) == nil {
			t.Errorf("validateJournaldKey(%q) = nil, want an error", k)
		}
	}
	for _, v := range []string{"", "10000", "30s", "info", "a b"} {
		if err := validateJournaldValue(v); err != nil {
			t.Errorf("validateJournaldValue(%q) = %v", v, err)
		}
	}
	for v, want := range map[string]string{
		" x":    "white space",
		"x ":    "white space",
		"x\\":   "backslash",
		"x\ny":  "single line",
		"x\x00": "single line",
	} {
		err := validateJournaldValue(v)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("validateJournaldValue(%q) = %v, want an error containing %q", v, err, want)
		}
	}
}

func TestJournaldSpecValidate(t *testing.T) {
	s := &journaldSpec{compress: ptr(true), extra: map[string]string{"Compress": "64K"}}
	if attr, err := s.validate(); attr != "extra" || err == nil || !strings.Contains(err.Error(), "also set by compress") {
		t.Errorf("validate() = %q, %v; want a conflict with compress", attr, err)
	}
	s = &journaldSpec{extra: map[string]string{"Compress": "64K", "Storage": "persistent"}}
	if attr, err := s.validate(); err != nil {
		t.Errorf("validate() = %q, %v", attr, err)
	}
	s = &journaldSpec{storage: "disk"}
	if attr, err := s.validate(); attr != "storage" || err == nil {
		t.Errorf("validate() = %q, %v; want an error for storage", attr, err)
	}
	s = &journaldSpec{extra: map[string]string{"lower": "x"}}
	if attr, err := s.validate(); attr != "extra" || err == nil {
		t.Errorf("validate() = %q, %v; want an error for extra", attr, err)
	}
}

func TestJournaldRender(t *testing.T) {
	s := &journaldSpec{
		storage: "persistent", systemMaxUse: "500M", maxRetentionSec: "1month",
		compress: ptr(true), forwardToSyslog: ptr(false),
		extra: map[string]string{"RateLimitBurst": "10000", "MaxFileSec": "1week", "Audit": ""},
	}
	want := journaldFileHeader + `
[Journal]
Storage=persistent
SystemMaxUse=500M
MaxRetentionSec=1month
Compress=yes
ForwardToSyslog=no
Audit=
MaxFileSec=1week
RateLimitBurst=10000
`
	if got := s.render(); got != want {
		t.Errorf("render() =\n%s\nwant\n%s", got, want)
	}
	if got, want := (&journaldSpec{}).render(), journaldFileHeader+"\n[Journal]\n"; got != want {
		t.Errorf("render() of an empty spec = %q, want %q", got, want)
	}
}

func TestParseJournaldFile(t *testing.T) {
	for _, s := range []*journaldSpec{
		{extra: map[string]string{}},
		{storage: "volatile", compress: ptr(false), extra: map[string]string{}},
		{systemMaxUse: "1G", maxRetentionSec: "2 weeks", forwardToSyslog: ptr(true), extra: map[string]string{"MaxLevelStore": "info"}},
	} {
		got, err := parseJournaldFile(s.render())
		if err != nil {
			t.Fatalf("parseJournaldFile(%q) = %v", s.render(), err)
		}
		if !reflect.DeepEqual(got, s) {
			t.Errorf("parseJournaldFile(render(%+v)) = %+v", s, got)
		}
	}

	// A file written by hand: comments, spacing, other boolean spellings,
	// a value only extra can hold, and a key assigned twice.
	got, err := parseJournaldFile(`; comment
# comment
[Journal]
 Storage = persistent
Compress=64K
ForwardToSyslog=off
SystemMaxUse=lots
RateLimitBurst=1
RateLimitBurst=2
Storage=auto
`)
	if err != nil {
		t.Fatal(err)
	}
	want := &journaldSpec{storage: "auto", forwardToSyslog: ptr(false),
		extra: map[string]string{"Compress": "64K", "SystemMaxUse": "lots", "RateLimitBurst": "2"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("parseJournaldFile = %+v, want %+v", got, want)
	}
	// A typed key that ends up valid drops its earlier extra value.
	got, err = parseJournaldFile("[Journal]\nCompress=64K\nCompress=yes\n")
	if err != nil {
		t.Fatal(err)
	}
	if want := (&journaldSpec{compress: ptr(true), extra: map[string]string{}}); !reflect.DeepEqual(got, want) {
		t.Errorf("parseJournaldFile = %+v, want %+v", got, want)
	}

	for content, want := range map[string]string{
		"":                               "no [Journal] section",
		"Storage=auto\n":                 "outside the [Journal] section",
		"[Journal]\n[Unit]\n":            "section [Unit] is not supported",
		"[Journal]\nStorage\n":           "not a key=value",
		"[Journal]\nMaxLevelStore=a\\\n": "continued lines",
		"[Journal]\nlower=x\n":           "extra",
	} {
		_, err := parseJournaldFile(content)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("parseJournaldFile(%q) = %v, want an error containing %q", content, err, want)
		}
	}
}
