package cli

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/DKPlugins/TokenResetsMonitor/internal/config"
)

func TestDoctorChecksEventPaginationContract(t *testing.T) {
	for _, valid := range []bool{false, true} {
		t.Run(map[bool]string{false: "missing_has_more", true: "complete_scan"}[valid], func(t *testing.T) {
			eventsRequested := false
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/providers":
					io.WriteString(w, `{"meta":{"schema_version":"1.0"},"data":[{"slug":"fixture","name":"Fixture"}]}`)
				case "/providers/fixture":
					io.WriteString(w, `{"meta":{"schema_version":"1.0"},"data":{"slug":"fixture","name":"Fixture"}}`)
				case "/providers/fixture/events":
					eventsRequested = true
					pagination := "{}"
					if valid {
						pagination = `{"has_more":false}`
					}
					io.WriteString(w, `{"meta":{"schema_version":"1.0"},"data":[],"pagination":`+pagination+`}`)
				default:
					t.Errorf("unexpected request: %s", r.URL)
					w.WriteHeader(404)
				}
			}))
			defer server.Close()
			cfg := config.Defaults()
			cfg.APIBaseURL = server.URL
			cfg.Providers = []config.ProviderFilter{{Slug: "fixture"}}
			last := doctorCheck{}
			checkAPIContract(context.Background(), cfg, func(name, status, message string) { last = doctorCheck{Name: name, Status: status, Message: message} })
			if !eventsRequested {
				t.Fatal("doctor skipped event endpoint")
			}
			if valid && last.Status != "ok" {
				t.Fatalf("valid contract failed: %+v", last)
			}
			if !valid && last.Status != "error" {
				t.Fatalf("malformed pagination accepted: %+v", last)
			}
		})
	}
}
