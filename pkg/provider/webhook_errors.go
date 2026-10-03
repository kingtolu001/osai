package provider

import "errors"

var (
	ErrMalformedWebhook    = errors.New("malformed provider webhook")
	ErrWebhookIgnored      = errors.New("unsupported provider webhook event")
	ErrProviderUnavailable = errors.New("provider webhook unavailable")
)
