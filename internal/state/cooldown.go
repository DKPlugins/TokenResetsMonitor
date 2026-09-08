package state

import (
	"time"

	bolt "go.etcd.io/bbolt"
)

func recipientKey(channel, destination string) string { return channel + "\x00" + destination }

func recipientCooldown(tx *bolt.Tx, channel, destination string) (time.Time, error) {
	var deadline time.Time
	_, err := getJSON(tx.Bucket([]byte("cooldowns")), recipientKey(channel, destination), &deadline)
	return deadline, err
}

// RecipientCooldown returns the durable throttle for one recipient fingerprint.
// A worker must check this immediately before sending each queued notification.
func (s *Store) RecipientCooldown(channel, destination string) (time.Time, error) {
	var deadline time.Time
	err := s.db.View(func(tx *bolt.Tx) error {
		var err error
		deadline, err = recipientCooldown(tx, channel, destination)
		return err
	})
	return deadline, err
}

func extendRecipientCooldown(tx *bolt.Tx, channel, destination string, deadline time.Time) error {
	current, err := recipientCooldown(tx, channel, destination)
	if err != nil {
		return err
	}
	if !deadline.After(current) {
		return nil
	}
	return putJSON(tx.Bucket([]byte("cooldowns")), recipientKey(channel, destination), deadline.UTC())
}
