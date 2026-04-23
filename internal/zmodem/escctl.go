package zmodem

// DetectEscctlFromZrinit inspects a rolling byte window for the receiver's
// ZRINIT hex header. Returns (escctlRequested, foundZrinit). Tolerates
// leading junk bytes and partial frames arriving across multiple reads.
//
// ESCCTL negotiation matters because old terminals (Amiga Term 4.8,
// NComm, xprzmodem.library) hang when we emit ZDLE-escaped control bytes
// they didn't ask for; lrzsz --escape hangs when we fail to emit the ones
// it did ask for.
func DetectEscctlFromZrinit(sniff []byte) (escctl bool, found bool) {
	if len(sniff) < 18 {
		return false, false
	}
	for i := 0; i <= len(sniff)-18; i++ {
		if sniff[i] != ZPAD || sniff[i+1] != ZPAD || sniff[i+2] != ZDLE || sniff[i+3] != ZHEX {
			continue
		}
		if !isAsciiHex(sniff[i+4 : i+18]) {
			continue
		}
		frame, flags, parsedOK := DecodeHexHeader(sniff, i+4)
		if !parsedOK || frame != FrameZRINIT {
			continue
		}
		// ZF0 (the capability byte) is the 4th flag byte on the wire.
		return flags[3]&EscCtl != 0, true
	}
	return false, false
}

func isAsciiHex(bs []byte) bool {
	for _, c := range bs {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')) {
			return false
		}
	}
	return true
}
