package state

import (
	"sort"
	"time"

	"github.com/DKPlugins/TokenResetsMonitor/internal/model"

	bolt "go.etcd.io/bbolt"
)

type ChannelExplanation struct {
	AcknowledgedRevision           int        `json:"acknowledged_revision,omitempty"`
	LatestRevision                 int        `json:"latest_revision"`
	AcknowledgedRevisionUnverified bool       `json:"acknowledged_revision_unverified,omitempty"`
	CurrentRecipientUnavailable    bool       `json:"current_recipient_unavailable,omitempty"`
	Channel                        string     `json:"channel"`
	Status                         string     `json:"status"`
	Reason                         string     `json:"reason"`
	NotificationID                 string     `json:"notification_id,omitempty"`
	EffectiveNextAttempt           *time.Time `json:"effective_next_attempt,omitempty"`
	CurrentRecipient               bool       `json:"current_recipient"`
	HistoricalUnavailable          bool       `json:"historical_unavailable,omitempty"`
}

func explainChannels(tx *bolt.Tx, record EventRecord, policy Policy) ([]ChannelExplanation, error) {
	known := map[string]bool{"webhook": true, "telegram": true, "slack": true}
	for channel := range policy.Channels {
		known[channel] = true
	}
	for channel := range record.DiscoveryChannels {
		known[channel] = true
	}
	for channel := range record.DeliveryIDs {
		known[channel] = true
	}
	for channel := range record.Acknowledged {
		known[channel] = true
	}
	names := make([]string, 0, len(known))
	for channel := range known {
		names = append(names, channel)
	}
	sort.Strings(names)
	explanations := make([]ChannelExplanation, 0, len(names))
	for _, channel := range names {
		item := ChannelExplanation{Channel: channel, Status: "not_queued", HistoricalUnavailable: record.DiscoveryUnavailable, CurrentRecipientUnavailable: policy.Channels == nil, LatestRevision: record.Event.Revision}
		ack, acknowledged := record.Acknowledged[channel]
		if acknowledged {
			item.AcknowledgedRevision = ack.Event.Revision
			item.AcknowledgedRevisionUnverified = ack.LegacyUnverified
		}
		if id := record.DeliveryIDs[channel]; id != "" {
			var delivery Delivery
			found, err := getJSON(tx.Bucket([]byte("outbox")), id, &delivery)
			if err != nil {
				return nil, err
			}
			if found {
				delivery, err = effectiveDelivery(tx, delivery)
				if err != nil {
					return nil, err
				}
				item.Status, item.NotificationID = delivery.Status, id
				item.CurrentRecipient = policy.Channels[channel] != "" && policy.Channels[channel] == delivery.Destination
				item.EffectiveNextAttempt = delivery.EffectiveNextAttempt
				switch delivery.Status {
				case "pending":
					item.Reason = "awaiting_delivery"
					if delivery.AttemptID != "" {
						item.Reason = "delivery_in_progress"
					}
				case "failed":
					item.Reason = "delivery_failed"
				case "delivered":
					item.Reason = "already_delivered"
				case "canceled":
					item.Reason = delivery.CancellationReason
					if item.Reason == "" {
						item.Reason = "delivery_canceled"
					}
				}
			}
		}
		if item.Status == "delivered" && acknowledged && policy.Channels != nil && len(model.MeaningfulChanges(ack.Event, record.Event)) > 0 {
			switch {
			case policy.Channels[channel] == "":
				item.Reason = "channel_disabled"
			case policy.Channels[channel] != ack.Destination:
				item.Reason = "recipient_changed"
			case !policy.Changes[channel]:
				item.Reason = "changes_disabled"
			default:
				item.Reason = "awaiting_source_revision"
			}
		}
		if item.Reason == "" {
			destination := policy.Channels[channel]
			item.CurrentRecipient = destination != "" && record.DiscoveryChannels[channel] == destination
			switch {
			case record.Baseline:
				item.Reason = "initial_history"
			case policy.Channels == nil:
				switch {
				case record.ChannelSuppressions[channel] != "":
					item.Reason = record.ChannelSuppressions[channel]
				case record.DiscoveryUnavailable:
					item.Reason = "legacy_decision_unavailable"
				case record.DiscoveryChannels[channel] == "":
					item.Reason = "channel_not_enabled_at_discovery"
				case !record.Decision.Matched:
					item.Reason = record.Decision.Reason
				default:
					item.Reason = "channel_state_unavailable"
				}
			case destination == "":
				item.Reason = "channel_disabled"
			case record.ChannelSuppressions[channel] != "":
				item.Reason = record.ChannelSuppressions[channel]
			case record.DiscoveryUnavailable:
				item.Reason = "legacy_decision_unavailable"
			case record.DiscoveryChannels[channel] == "":
				item.Reason = "channel_not_enabled_at_discovery"
			case record.DiscoveryChannels[channel] != destination:
				item.Reason = "recipient_changed"
			case !record.Decision.Matched:
				item.Reason = record.Decision.Reason
			default:
				item.Reason = "awaiting_source_revision"
			}
		}
		explanations = append(explanations, item)
	}
	return explanations, nil
}
