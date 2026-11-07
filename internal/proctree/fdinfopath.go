package proctree

import "bytes"

// fdPathFromVnodeFdInfo decodes vip_path out of a vnode_fdinfowithpath
// buffer produced by proc_pidfdinfo(PROC_PIDFDVNODEPATHINFO): a 24-byte
// proc_fileinfo header, then vnode_info_path — 152-byte vnode_info followed
// by the MAXPATHLEN path buffer, which starts at offset 176.
//
// It returns ("", false) when the buffer is the wrong length or the tail is
// not a plausible absolute path: a truncated, zeroed, or malformed result
// must look like "fd has no path", never like a path.
//
// Layout notes live here, in a file with no build tag, so the decode itself
// is exercised by the Linux CI tests that cover this package's byte-level
// contracts (like cwdFromVnodePathInfo in vnodepathinfo.go).
const (
	vnodeFdInfoWithPathSize = 1200 // sizeof(struct vnode_fdinfowithpath)
	vnodeFdInfoPathOffset   = 176  // offsetof(pvip.vip_path)
)

func fdPathFromVnodeFdInfo(buf []byte) (string, bool) {
	if len(buf) != vnodeFdInfoWithPathSize {
		return "", false
	}
	raw := buf[vnodeFdInfoPathOffset : vnodeFdInfoPathOffset+vnodePathInfoMaxPathLen]
	if i := bytes.IndexByte(raw, 0); i >= 0 {
		raw = raw[:i]
	}
	if len(raw) == 0 || raw[0] != '/' {
		return "", false
	}
	for _, c := range raw {
		if c < 0x20 || c == 0x7f {
			return "", false
		}
	}
	return string(raw), true
}
