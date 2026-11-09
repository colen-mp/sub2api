package ctxkey

// UserEmail is the Sub2API user's email resolved by API key authentication.
// Client-supplied user identifiers must never populate this key.
const UserEmail Key = "ctx_user_email"

// TelemetryAPIKeyID is the authenticated numeric key ID, never the API secret.
const TelemetryAPIKeyID Key = "ctx_telemetry_api_key_id"

// TelemetryAPIKeyName is the stored name of the authenticated key, never its secret.
const TelemetryAPIKeyName Key = "ctx_telemetry_api_key_name"

// TelemetryUserName is the stored username resolved by API key authentication.
const TelemetryUserName Key = "ctx_telemetry_user_name"
