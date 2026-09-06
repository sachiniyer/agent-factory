package app

func helpProductTitle() string {
	if Version == "" {
		return "Agent Factory"
	}
	return "Agent Factory v" + Version
}
