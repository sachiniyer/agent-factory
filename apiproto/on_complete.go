package apiproto

// ListOnCompleteRequest asks for the spawned-session lifecycle choices.
type ListOnCompleteRequest struct{}

// OnCompleteOption is a wire value and its shared consequence text.
type OnCompleteOption struct {
	Value string `json:"value"`
	Hint  string `json:"hint"`
}

// ListOnCompleteResponse preserves the daemon's least-destructive-first order.
type ListOnCompleteResponse struct {
	Values []OnCompleteOption `json:"values"`
}
