package apiclient

// CallProposal calls one proposal route, for example "ListProposals", on the
// daemon this client targets.
func (c *Client) CallProposal(method string, req, resp any) error {
	return c.call(method, req, resp)
}
