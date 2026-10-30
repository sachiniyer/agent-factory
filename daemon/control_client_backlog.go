package daemon

// CallBacklog calls one backlog route on the local daemon, starting it when
// needed. method is the route verb, for example "GetBacklog".
func CallBacklog(method string, req, resp any) error {
	if !backlogMethods[method] {
		return errUnknownBacklogMethod(method)
	}
	return callDaemon(method, req, resp)
}

var backlogMethods = map[string]bool{
	"GetBacklog": true, "AddBacklogItems": true, "UpdateBacklogItem": true, "MoveBacklogItem": true,
	"SetBacklogItemStatus": true, "DeleteBacklogItem": true, "PostBacklogMessage": true,
	"SetBacklogSettings": true, "GetBudget": true, "SetBudget": true,
}
