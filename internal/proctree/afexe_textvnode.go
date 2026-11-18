package proctree

// This file holds the platform-independent half of darwin's text-vnode
// attestation: the consensus that folds a process's vnode-backed regions
// into one (dev, ino) identity. It carries NO build tag, deliberately —
// the same reason vnodepathinfo.go does not. The defect the consensus
// refuses lives in arithmetic and a comparison over a decoded struct, not
// in a syscall, so every runner can check it and a darwin-only regression
// can never slip past a Linux-only CI leg.
//
// The syscall-driven cursor walk itself (afexe_darwin.go exeTextVnode) is
// the only piece that needs a real darwin kernel — exactly the one piece
// CI's test-macos job exercises. Keeping the fold and the layout out of
// the walk lets Linux CI run the poisoned-walk verdict without staging a
// process behind a fake proc_info, the seam the original design never
// had.

// Layout of struct proc_regionwithpathinfo (bsd/sys/proc_info.h), derived
// in the same spirit as vnodepathinfo.go: proc_regioninfo is 96 bytes
// (pri_protection uint32 @0, pri_address uint64 @80, pri_size uint64 @88),
// then vnode_info_path — vi_stat (vst_dev uint32 @96, vst_ino uint64 @104,
// inside the 136-byte stat block) then vip_path[MAXPATHLEN] at 96+152 =
// 248. afexe_textvnode_test.go re-derives every offset from the SDK field
// table so a typo fails a test rather than reading the wrong bytes — the
// same discipline vnodepathinfo.go pins the cwd path offsets with.
const (
	procRegionWithPathInfoSize = 1272
	// regionPriProtectionOff is offsetof(proc_regioninfo.pri_protection):
	// the region's CURRENT vm protection, a vm_prot_t bitset
	// (VM_PROT_READ | VM_PROT_WRITE | VM_PROT_EXECUTE). xnu's
	// PROC_PIDREGIONPATHINFO2 — fill_procregioninfo_onlymappedvnodes /
	// fill_vnodeinfoforaddr — fills this for every vnode-backed mapping it
	// reports, with NO filter on it and no binding to p_textvp, so the
	// walk sees a PROT_READ-only mmap of any regular file alongside the
	// executed image's PROT_EXEC text. pri_protection is the FIRST field
	// of proc_regioninfo, so its offset is 0.
	regionPriProtectionOff = 0
	regionPriAddressOff    = 80
	regionPriSizeOff       = 88
	regionVnodeDevOff      = 96
	regionVnodeInoOff      = 104
	regionPathOff          = 248
	regionPathLen          = 1024
	// regionScanBound caps the cursor walk so a hostile or pathological map
	// can never make the attestation read unbounded.
	regionScanBound = 16384
)

// regionMatch is one vnode-backed region whose vip_path equals the
// kernel-attested exe path — a candidate for the text vnode. exeTextVnode
// decodes these out of darwin's PROC_PIDREGIONPATHINFO2 buffer and folds
// them with collectTextVnode. The fields are the parts of
// proc_regionwithpathinfo the consensus needs: the backing vnode's
// (dev, ino) and the region's CURRENT vm protection.
type regionMatch struct {
	dev        uint64
	ino        uint64
	protection uint32
}

// vmProtExecute is VM_PROT_EXECUTE from mach/vm_prot.h — the bit
// pri_protection sets on a region the kernel maps with execute access
// (0x01 = READ, 0x02 = WRITE, 0x04 = EXECUTE). The text segment of an
// executed image is PROT_READ|PROT_EXEC; a pure PROT_READ mmap of a
// foreign inode can never carry it. Held here as an opaque filter bit
// rather than a syscall-derived constant so the consensus is testable on
// platforms that have no mach vm_prot.h.
const vmProtExecute uint32 = 0x04

// collectTextVnode folds the regions whose vip_path strings equal the
// kernel-attested exe path into one text-vnode identity, reporting false
// whenever the walk is ambiguous — the fail-closed direction. Its
// contract matches exeTextVnode's: a single positive answer is the
// (dev, ino) of the inode the kernel is executing.
//
// Three things must hold, each a way a poisoned map is caught rather than
// believed (xnu's PROC_PIDREGIONPATHINFO2 reports ANY vnode-backed regular
// mapping, with no filter on protection and no binding to p_textvp — see
// afexe_darwin.go exeTextVnode):
//
//   - at least one match. A map with no region naming the exe path is the
//     process-not-executing-what-the-name-says case — foreign, unverified.
//   - at least one match must be PROT_EXEC. The executed image's own
//     __TEXT is PROT_EXEC; a non-exec mapping naming the exe path — a
//     process that maps an af build under the right path without ever
//     executing it — cannot be the running image, and the verdict refuses.
//   - all matches must agree on the same (dev, ino). A benign executable
//     names only its text vnode, across __TEXT/__DATA/__LINKEDIT — one
//     inode. A SECOND inode answering the same path is a foreign inode the
//     process itself mapped under the exe path: the only witness an
//     unfiltered walk has, and a forced fail-closed.
//
// The first match sets the expected identity, and the verdict is symmetric
// in match order — the kernel reports regions in address order, and an
// attacker who places the poison mapping ABOVE the text region (the only
// choice once the text sits low) trips the check the same way. The fold is
// fail-closed, never "first match wins": a poisoned walk that won the
// cursor order used to authorize the close (the accepted #7494 item-3 cost
// floor "the attacker must RUN a real af build") — now it reads as
// unverified, the same answer the reaper already gives for any
// unverifiable daemon.
func collectTextVnode(matches []regionMatch) (dev uint64, ino uint64, ok bool) {
	if len(matches) == 0 {
		return 0, 0, false
	}
	dev, ino = matches[0].dev, matches[0].ino
	var sawExec bool
	for _, m := range matches {
		if m.dev != dev || m.ino != ino {
			return 0, 0, false
		}
		if m.protection&vmProtExecute != 0 {
			sawExec = true
		}
	}
	if !sawExec {
		return 0, 0, false
	}
	return dev, ino, true
}
