package apiclient

// CallBacklog calls one backlog route, for example "GetBacklog", on the daemon
// this client targets.
func (c *Client) CallBacklog(method string, req, resp any) error {
	return c.call(method, req, resp)
}
