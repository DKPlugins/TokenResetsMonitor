package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"text/template"
	"text/template/parse"
	"time"

	"github.com/DKPlugins/TokenResetsMonitor/internal/model"
)

const MaxTemplateBytes = 1 << 20
const maxTemplateSteps = 10000

type templateBuffer struct{ bytes.Buffer }

func (b *templateBuffer) Write(p []byte) (int, error) {
	if b.Len()+len(p) > MaxTemplateBytes {
		return 0, errors.New("template output too large")
	}
	return b.Buffer.Write(p)
}
func (b *templateBuffer) WriteString(s string) (int, error) { return b.Write([]byte(s)) }

// RenderTemplate is shared by validation and delivery so neither can silently
// accept a template that the other parses differently.
func RenderTemplate(body string, n model.Notification) ([]byte, error) {
	if len(body) > MaxTemplateBytes {
		return nil, errors.New("webhook body template exceeds size limit (content omitted)")
	}
	steps := maxTemplateSteps
	remainingWork := 4 * MaxTemplateBytes
	charge := func(work int) error {
		if work > MaxTemplateBytes || work > remainingWork {
			return errFormatBudget
		}
		remainingWork -= work
		return nil
	}
	printValues := func(newline bool, args []any) (string, error) {
		work := len(args)
		for _, value := range args {
			n, _ := formatValueWork(reflect.ValueOf(value), 0)
			work += n
			if work > MaxTemplateBytes {
				return "", errFormatBudget
			}
		}
		if err := charge(work); err != nil {
			return "", err
		}
		result := ""
		if newline {
			result = fmt.Sprintln(args...)
		} else {
			result = fmt.Sprint(args...)
		}
		if len(result) > MaxTemplateBytes {
			return "", errFormatBudget
		}
		return result, nil
	}
	escapeValues := func(escape func(string) string, args []any) (string, error) {
		value, err := printValues(false, args)
		if err != nil {
			return "", err
		}
		if err := charge(6 * len(value)); err != nil {
			return "", err
		}
		return escape(value), nil
	}
	funcs := template.FuncMap{
		"json": func(v any) (string, error) {
			work, _ := formatValueWork(reflect.ValueOf(v), 0)
			if err := charge(work); err != nil {
				return "", err
			}
			data, err := json.Marshal(v)
			if len(data) > MaxTemplateBytes {
				return "", errFormatBudget
			}
			return string(data), err
		},
		"printf": func(format string, args ...any) (string, error) {
			work, err := formatWork(format, args)
			if err != nil {
				return "", err
			}
			if err := charge(work); err != nil {
				return "", err
			}
			result := fmt.Sprintf(format, args...)
			if len(result) > MaxTemplateBytes {
				return "", errFormatBudget
			}
			return result, nil
		},
		"print":   func(args ...any) (string, error) { return printValues(false, args) },
		"println": func(args ...any) (string, error) { return printValues(true, args) },
		"html":    func(args ...any) (string, error) { return escapeValues(template.HTMLEscapeString, args) },
		"js":      func(args ...any) (string, error) { return escapeValues(template.JSEscapeString, args) },
		"urlquery": func(args ...any) (string, error) {
			return escapeValues(func(s string) string { return template.URLQueryEscaper(s) }, args)
		},
		"__trm_step": func() (string, error) {
			steps--
			if steps < 0 {
				return "", errors.New("template execution budget exceeded")
			}
			return "", nil
		},
	}
	t, err := template.New("webhook").Option("missingkey=error").Funcs(funcs).Parse(body)
	if err != nil {
		return nil, errors.New("webhook body template is invalid (content omitted)")
	}
	// A byte limit alone cannot stop empty integer ranges or recursive templates.
	// Charge each executed list (including each range iteration) before user work.
	guard := template.Must(template.New("budget").Funcs(funcs).Parse("{{__trm_step}}"))
	action := guard.Tree.Root.Nodes[0]
	seen := make(map[*parse.ListNode]bool)
	for _, associated := range t.Templates() {
		if associated.Tree != nil {
			boundTemplateLists(associated.Tree.Root, action, seen)
		}
	}
	var out templateBuffer
	if err := t.Execute(&out, n); err != nil {
		return nil, errors.New("webhook body template failed to render or exceeds size limit (content omitted)")
	}
	return out.Bytes(), nil
}

func ValidateTemplate(body string) error {
	if body == "" {
		return nil
	}
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	n := model.Notification{SchemaVersion: 1, ID: "validation", DetectedAt: now,
		Event: model.Event{ID: "validation", Slug: "validation", Provider: model.Provider{Slug: "openai-codex", Name: "Codex"},
			Title: "Validation", EventType: "hard_reset", Status: "published", Revision: 1, AnnouncedAt: now,
			Scope:      model.Scope{Plans: []string{"plus"}, Products: []string{"codex"}, Windows: []string{"weekly"}, Evidence: "unknown"},
			Confidence: model.Confidence{Label: "reported", Score: 0.5}, Links: model.Links{HTML: "https://tokenresets.com/"}}}
	// Cover both production/test and optional dates absent/present. Templates
	// must guard optional values rather than permanently failing real deliveries.
	for _, test := range []bool{false, true} {
		n.Test = test
		n.Event.PublishedAt, n.Event.EffectiveAt = nil, nil
		n.Event.ExpectedBy, n.Event.ObservedEffectiveAt, n.Event.ExpiresAt = nil, nil, nil
		if _, err := RenderTemplate(body, n); err != nil {
			return err
		}
		n.Event.PublishedAt, n.Event.EffectiveAt = &now, &now
		n.Event.ExpectedBy, n.Event.ObservedEffectiveAt, n.Event.ExpiresAt = &now, &now, &now
		if _, err := RenderTemplate(body, n); err != nil {
			return err
		}
	}
	return nil
}

func boundTemplateLists(list *parse.ListNode, guard parse.Node, seen map[*parse.ListNode]bool) {
	if list == nil || seen[list] {
		return
	}
	seen[list] = true
	for _, node := range list.Nodes {
		switch n := node.(type) {
		case *parse.IfNode:
			boundTemplateLists(n.List, guard, seen)
			boundTemplateLists(n.ElseList, guard, seen)
		case *parse.RangeNode:
			boundTemplateLists(n.List, guard, seen)
			boundTemplateLists(n.ElseList, guard, seen)
		case *parse.WithNode:
			boundTemplateLists(n.List, guard, seen)
			boundTemplateLists(n.ElseList, guard, seen)
		}
	}
	list.Nodes = append([]parse.Node{guard.Copy()}, list.Nodes...)
}
