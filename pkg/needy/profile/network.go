package profile

// ActorNetwork is the actor's network, which reaches the outside only
// through the actor gateway.
type ActorNetwork struct {
	// Allow lists host globs the actor may reach.
	Allow []string `json:"allow,omitzero"`
	// Services are plain-HTTP services the actor reaches as `NAME.needy`.
	Services map[string]Service `json:"services,omitzero"`
}

// Service is a plain-HTTP service the gateway forwards to.
type Service struct {
	// URL is the base URL requests go to, resolved on the control host.
	URL *string `json:"url,omitzero"`
}

// NetworkPolicy is the executor network.
type NetworkPolicy struct {
	// Allow lists host globs executors may reach.
	Allow []string `json:"allow,omitzero"`
	// Record records the executors' connections.
	Record *bool `json:"record,omitzero"`
}
