package apiproto

// ListOnCompleteRequest asks for the spawned-session lifecycle choices.
type ListOnCompleteRequest struct{}

// ListOnCompleteResponse preserves the daemon's least-destructive-first order.
type ListOnCompleteResponse struct {
	Values []string `json:"values"`
}
