package main

import (
	"crypto/rand"
	"io"
	"log"
	"net"
	"os"
	"syscall"
)

func checkNetErr(err error) bool {
	if err == nil {
		return false
	}
	if e, ok := err.(*net.OpError); ok {
		if e, ok := e.Err.(*os.SyscallError); ok &&
			e.Err == syscall.ECONNREFUSED {

			return true
		}
	}
	log.Fatal("Network error: ", err)
	panic(nil)
}

const headerLen = 8 + 1 + 1 + 2

type header struct {
	Id      uint64 // Schould have a random initial value.
	FragN   byte
	FragNum byte
	Len     uint16
}

func (h *header) Encode(buf []byte) {
	id := h.Id
	for i := 0; i < 8; i++ {
		buf[i] = byte(id & 0xff)
		id >>= 8
	}
	buf[8] = h.FragN
	buf[9] = h.FragNum
	buf[10] = byte(h.Len & 0xff)
	buf[11] = byte(h.Len >> 8)
}

func (h *header) Decode(buf []byte) {
	for i := 7; i >= 0; i-- {
		h.Id <<= 8
		h.Id |= uint64(buf[i])
	}
	h.FragN = buf[8]
	h.FragNum = buf[9]
	h.Len = uint16(buf[10]) | uint16(buf[11])<<8
}

func blkAlignUp(n int) int {
	return (n + blkMask) &^ blkMask
}

func tunRead(tun io.Reader, con io.Writer, cfg *config) {
	buffer := make([]byte, 8192)
	_, err := rand.Read(buffer[:8])
	checkErr(err)
	var h header
	for _, b := range buffer[:8] {
		h.Id = h.Id<<8 | uint64(b)
	}
	pkt := make([]byte, headerLen+cfg.MaxPay+2*blkCipher.BlockSize())
	for {
		buf := buffer
		n, err := tun.Read(buf[headerLen:])
		checkErr(err)
		if n == 0 {
			continue
		}

		h.FragNum = byte((n + cfg.MaxPay - 1) / cfg.MaxPay)

		// Equally fill all h.FragNum packets.
		payLen := (n + int(h.FragNum) - 1) / int(h.FragNum)
		usedLen := headerLen + payLen
		pktLen := blkAlignUp(usedLen)

		buf = buf[:n+headerLen]

		for h.FragN = 0; h.FragN < h.FragNum; h.FragN++ {
			if len(buf) < usedLen {
				usedLen = len(buf)
				payLen = usedLen - headerLen
				pktLen = blkAlignUp(usedLen)
			}
			h.Len = uint16(payLen)

			log.Printf("%d\n", h)
			h.Encode(buf)

			copy(pkt, buf[:usedLen]) // Encrypt here.

			_, err := con.Write(pkt[:pktLen])
			if checkNetErr(err) {
				break
			}
			buf = buf[payLen:]
		}
		h.Id++
	}
}

/*func getMTU(iname string) int {
	dev, err := net.InterfaceByName(iname)
	checkErr(err)
	return dev.MTU
}*/

type defrag struct {
	Id    uint64
	Frags [][]byte
}

func tunWrite(tun io.Writer, con io.Reader, cfg *config) {
	buf := make([]byte, 8192)
	dtab := make([]*defrag, 3)
	for i := range dtab {
		dtab[i] = &defrag{Frags: make([][]byte, 0, (8192+cfg.MaxPay-1)/cfg.MaxPay)}
	}
	var h header
	for {
		n, err := con.Read(buf)
		checkNetErr(err)
		switch {
		case n >= headerLen+20:
			// Decrypt here.

			h.Decode(buf)
			pktLen := blkAlignUp(headerLen + int(h.Len))
			if n != pktLen {
				log.Printf("%s: Bad packet size: %d != %d.", cfg.Dev, n, pktLen)
				continue
			}
			if h.FragNum > 1 {
				var (
					cur *defrag
					cn  int
				)
				for i, d := range dtab {
					if d.Id == h.Id {
						cur = d
						cn = i
						break
					}
				}
				if cur == nil {
					cur = dtab[len(dtab)-1]
					copy(dtab[1:], dtab)
					dtab[0] = cur
				}
				if len(cur.Frags) != int(h.FragNum) {
					if cap(cur.Frags) < int(h.FragNum) || h.FragN >= h.FragNum ||
						len(cur.Frags) != 0 && len(cur.Frags) != int(h.FragNum) {

						log.Printf("%s: Bad header %d", cfg.Dev, h)
						continue
					}
					if len(cur.Frags) == 0 {
						cur.Id = h.Id
						cur.Frags = cur.Frags[:h.FragNum]
					}
				}
				frag := cur.Frags[h.FragN]
				if frag == nil {
					frag = make([]byte, 0, cfg.MaxPay)
				}
				frag = frag[:h.Len]
				copy(frag, buf[headerLen:])
				for _, frag = range cur.Frags {
					if frag == nil {
						// Found lack of fragment.
						break
					}
				}
				if frag == nil {
					// Lack of some fragment.
					continue
				}
				// All fragments received.
				n = 0
				for i, frag := range cur.Frags {
					n += copy(buf[n:], frag)
					cur.Frags[i] = frag[:0]
				}
				cur.Frags = cur.Frags[:0]
				copy(dtab[cn:], dtab[cn+1:])
				dtab[len(dtab)-1] = cur
			}
			_, err := tun.Write(buf[:n])
			if pathErr, ok := err.(*os.PathError); ok &&
				pathErr.Err == syscall.EINVAL {

				log.Printf("%s: Invalid IP datagram.\n", cfg.Dev)
				break
			}
			checkErr(err)

		case n > 0:
			log.Printf("%s: Received packet is to short.", cfg.Dev)
		}
	}
}
