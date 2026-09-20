package webhookcore

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"time"
)

func SignOutboundRequest(secret, timestamp string, body []byte) (string, error) {
	if secret == "" || timestamp == "" {
		return "", errors.New("secret and timestamp are required")
	}
	mac := hmac.New(sha256.New, []byte(secret))
	_, err := mac.Write([]byte(timestamp + "."))
	if err != nil {
		return "", err
	}
	_, err = mac.Write(body)
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(mac.Sum(nil)), nil
}

func VerifyOutboundRequest(secret, timestamp string, body []byte, signature string) (bool, error) {
	if secret == "" || timestamp == "" {
		return false, errors.New("secret and timestamp are required")
	}
	if _, err := time.Parse(time.RFC3339, timestamp); err != nil {
		if _, err2 := time.Parse(time.RFC3339Nano, timestamp); err2 != nil {
			return false, fmt.Errorf("invalid timestamp: %w", err)
		}
	}
	if signature == "" {
		return false, errors.New("missing signature")
	}
	expected, err := SignOutboundRequest(secret, timestamp, body)
	if err != nil {
		return false, err
	}
	if !hmac.Equal([]byte(expected), []byte(signature)) {
		return false, nil
	}
	return true, nil
}
