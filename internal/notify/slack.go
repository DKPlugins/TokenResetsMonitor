package notify

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/DKPlugins/TokenResetsMonitor/internal/config"
	"github.com/DKPlugins/TokenResetsMonitor/internal/model"
)

func renderSlack(c config.Slack, n model.Notification, r *rendered) error {
	if !validURL(c.WebhookURL) {
		return errors.New("Slack webhook URL is invalid")
	}
	r.endpoint, r.method = c.WebhookURL, http.MethodPost
	var err error
	r.timeout, err = duration(c.Timeout)
	if err != nil {
		return err
	}
	r.body, err = json.Marshal(struct {
		Text        string `json:"text"`
		Mrkdwn      bool   `json:"mrkdwn"`
		UnfurlLinks bool   `json:"unfurl_links"`
		UnfurlMedia bool   `json:"unfurl_media"`
	}{Text: slackText(n)})
	return err
}

func decodeSlack(body io.Reader, result *model.DeliveryResult) {
	if result.StatusCode != http.StatusOK {
		result.Retryable = result.StatusCode == http.StatusTooManyRequests || result.StatusCode >= 500
		result.Error = fmt.Sprintf("Slack returned HTTP %d; verify the incoming webhook and its workspace/channel", result.StatusCode)
		return
	}
	data, err := io.ReadAll(io.LimitReader(body, 4097))
	if err != nil || len(data) > 4096 || strings.TrimSpace(string(data)) != "ok" {
		result.Error = "Slack returned an invalid acknowledgement"
		result.Retryable = true
		return
	}
	result.Success = true
}

// Escape control characters even with mrkdwn disabled: source text cannot
// create channel-wide or user mentions.
func slackText(n model.Notification) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(telegramText(n))
}
