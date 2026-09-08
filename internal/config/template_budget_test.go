package config

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/DKPlugins/TokenResetsMonitor/internal/model"
)

func TestTemplateExecutionBudgetStopsEmptyLoopsAndRecursion(t *testing.T) {
	for _, body := range []string{
		"{{range 1000000000}}{{end}}",
		`{{define "loop"}}{{template "loop" .}}{{end}}{{template "loop" .}}`,
		`{{range 1000}}{{range 1000}}{{end}}{{end}}`,
		`{{range 1000000000}}{{continue}}{{end}}`,
		`{{with .Event}}{{range 1000000000}}{{end}}{{end}}`,
		`{{block "loop" .}}{{range 1000000000}}{{continue}}{{end}}{{end}}`,
	} {
		if _, err := RenderTemplate(body, model.Notification{}); err == nil {
			t.Fatal("unbounded empty template accepted")
		} else if strings.Contains(err.Error(), body) {
			t.Fatal("template content leaked")
		}
	}
	output, err := RenderTemplate(`{{range 3}}ok{{end}}`, model.Notification{})
	if err != nil || string(output) != "okokok" {
		t.Fatalf("bounded loop failed: %q %v", output, err)
	}
}

func TestFormattingWorkIsBoundedBeforeIntermediateAllocation(t *testing.T) {
	n := model.Notification{}
	n.Event.Scope.Plans = []string{"first", "second"}
	n.Event.Title = strings.Repeat("x", 100000)
	formats := []string{
		"{{printf " + fmt.Sprintf("%q", strings.Repeat("%1000000s", 40)) + strings.Repeat(" \"\"", 40) + "}}",
		`{{printf "%*s%*s" 1000000 "" 1000000 ""}}`,
		`{{printf "%[1]*[2]s%[1]*[2]s" 1000000 ""}}`,
		`{{printf "%1000000v" .Event.Scope.Plans}}`,
		`{{range 100}}{{$discard := printf "%300000s" ""}}{{end}}`,
		`{{print .Event.Title .Event.Title .Event.Title}}`,
		`{{html .Event.Title .Event.Title .Event.Title}}`,
		`{{js .Event.Title .Event.Title .Event.Title}}`,
		`{{urlquery .Event.Title .Event.Title .Event.Title}}`,
	}
	for _, body := range formats {
		if _, err := RenderTemplate(body, n); err == nil {
			t.Fatal("unbounded formatting work accepted")
		}
	}
}

func TestBoundedFormattingRetainsOrdinaryTemplateBehavior(t *testing.T) {
	n := model.Notification{}
	n.Event.AnnouncedAt = time.Date(2026, 9, 8, 1, 2, 3, 0, time.UTC)
	cases := []struct{ body, want string }{
		{`{{html "<b>"}}`, "&lt;b&gt;"},
		{`{{urlquery "a b"}}`, "a+b"},
		{`{{printf "%08d %.2f" 42 3.14159}}`, fmt.Sprintf("%08d %.2f", 42, 3.14159)},
		{`{{printf "%[2]d %[1]s" "hello" 42}}`, fmt.Sprintf("%[2]d %[1]s", "hello", 42)},
		{`{{printf "%[3]*.[2]*[1]f" 12.345 2 8}}`, fmt.Sprintf("%[3]*.[2]*[1]f", 12.345, 2, 8)},
		{`{{printf "%*s %d" 5 "hi" 1777777777}}`, fmt.Sprintf("%*s %d", 5, "hi", 1777777777)},
		{`{{printf "%s" (.Event.AnnouncedAt.Format "2006-01-02")}}`, "2026-09-08"},
		{`{{print "a" 2 3}}{{println "x" 4}}`, fmt.Sprint("a", 2, 3) + fmt.Sprintln("x", 4)},
		{`{{printf "100%% %q" "hello"}}`, fmt.Sprintf("100%% %q", "hello")},
	}
	for _, tc := range cases {
		output, err := RenderTemplate(tc.body, n)
		if err != nil || string(output) != tc.want {
			t.Fatalf("%s: %q want %q, %v", tc.body, output, tc.want, err)
		}
	}
	output, err := RenderTemplate(`{{json .}}`, n)
	if err != nil || !json.Valid(output) {
		t.Fatalf("notification JSON helper broke: %s %v", output, err)
	}
}
