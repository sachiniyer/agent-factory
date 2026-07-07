package session

func (hp *hookPTY) closeStdout() {
	if hp == nil || hp.stdout == nil {
		return
	}
	_ = hp.stdout.Close()
}
