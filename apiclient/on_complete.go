package apiclient

import "github.com/sachiniyer/agent-factory/apiproto"

// ListOnComplete returns task lifecycle choices in daemon presentation order.
func (c *Client) ListOnComplete() ([]string, error) {
	var resp apiproto.ListOnCompleteResponse
	if err := c.call("ListOnComplete", apiproto.ListOnCompleteRequest{}, &resp); err != nil {
		return nil, err
	}
	return resp.Values, nil
}
