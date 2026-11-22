// Event types of the trigger sources that the control plane itself runs
// (schedules and inbound webhooks). Port of internal/triggers/attributes.go.
// The attribute keys and the providers are EVENT_ATTR and EVENT_PROVIDER of
// @openfactory/core; the contract per provider and event is in ATTRIBUTES.md
// next to this file.

export const EventCronFired = "cron_fired";
export const EventWebhookReceived = "received";
