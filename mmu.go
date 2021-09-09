package main

import (
	"errors"
	"unsafe"
)

// Perm represent permissions of memory addresses
type Perm uint8

// Enum of permission variants supported an another variant for keeping
// track of modified memory locations.
const (
	PERM_READ  Perm = 1 << 0 // read permission
	PERM_WRITE      = 1 << 1 // write permission
	PERM_EXEC       = 1 << 2 // executable permission
	PERM_RAW        = 1 << 3 // read-after-write permission

	DIRTY_BLOCK_SIZE = 0x7f
)

var (
	ErrMemIONotPermitted = errors.New("mmu: you don't perms to access memory")
)

// VirtAddr is a guest virtual address and an io.ReadWriter
type VirtAddr uint

// Block is a block of memory, it maps the start of the block to the end
// of the block
type Block = map[VirtAddr]VirtAddr

// Mmu is an isolated memory space
type Mmu struct {
	// memory is blob of memory space available to the system
	memory []uint8
	// access restrictions on individual locations in memory
	permissions []Perm
	// map of modified blocks of memory
	dirty Block
	// tracks the current allocation
	curAlloc VirtAddr
}

func NewMmu(size uint) *Mmu {
	return &Mmu{
		memory:      make([]uint8, size),
		permissions: make([]Perm, size),
		dirty:       make(Block),
		curAlloc:    VirtAddr(0x800),
	}
}

// Reset restores all memory back to the original state.
func (m *Mmu) Reset(other *Mmu) {
	for addr, endAddr := range m.dirty {
		start := int(addr)
		end := int(endAddr)

		// restore memory state
		copy(m.memory[start:end], other.memory[start:end])
		// restore permissions
		copy(m.permissions[start:end], other.permissions[start:end])
	}
	// clear dirty list
	m.dirty = make(Block)
}

// Fork an existing Mmu
func (m *Mmu) Fork() *Mmu {
	mmu := &Mmu{
		memory:      append(make([]uint8, 0, len(m.memory)), m.memory...),
		permissions: append(make([]Perm, 0, len(m.permissions)), m.permissions...),
		dirty:       make(Block),
		curAlloc:    m.curAlloc,
	}
	return mmu
}

// Allocate allocates region of memory as RW in the address space
func (m *Mmu) Allocate(size uint) VirtAddr {
	// 16-byte align the allocation
	alignSize := (size + 0xf) &^ 0xf

	// get the base addr
	base := m.curAlloc

	// allocation is bigger than available memory
	if int(base) >= len(m.memory) {
		return 0
	}

	// update current allocation size
	m.curAlloc += VirtAddr(alignSize)

	// could not satisfy allocation without going out of memory
	if int(m.curAlloc) > len(m.memory) {
		// abort allocation and revert back to base
		m.curAlloc = base
		return 0
	}

	// mark memory as uninitialized and writable
	m.SetPermissions(base, size, PERM_RAW|PERM_WRITE)

	return base
}

// SetPermission sets the required permissions on memory locations starting
// from the	`addr` to `addr+size`
func (m *Mmu) SetPermissions(addr VirtAddr, size uint, perm Perm) {
	// set permissions for the allocated memory
	for i := uint(addr); i < uint(addr)+size; i++ {
		m.permissions[i] = perm
	}
}

// WriteFrom copies the buffer `buf` into memory checking the necessary
// permission before doing so
func (m *Mmu) WriteFrom(addr VirtAddr, buf []uint8) error {
	//get the permission on the region of memory to write to
	perms := m.permissions[int(addr) : len(buf)+int(addr)]

	hasRAW := false
	for _, p := range perms {
		// check if any part of the memory has is read-after-write
		hasRAW = hasRAW || ((p & PERM_RAW) != 0)

		// check if all perms are set to write
		if (p & PERM_WRITE) == 0 {
			return ErrMemIONotPermitted
		}
	}

	// copy the slice `buf` into memory pointed to by `addr`
	n := copy(m.memory[int(addr):len(buf)+int(addr)], buf)

	// update permissions and allow reading after writing
	if hasRAW {
		for i, p := range perms {
			if (p & PERM_RAW) != 0 {
				perms[i] |= PERM_READ
			}
		}
	}

	// update the dirty block map
	// aligned block to keep track of modified memory
	blockStart := (int(addr) + DIRTY_BLOCK_SIZE) &^ DIRTY_BLOCK_SIZE
	round := DIRTY_BLOCK_SIZE + 1
	if blockStart > int(addr) {
		blockStart -= round
	}
	// align block to the dirty block size
	numBlocks := int((n+DIRTY_BLOCK_SIZE)&^DIRTY_BLOCK_SIZE) / round
	// end index of the aligned block
	blockEnd := VirtAddr(blockStart + (numBlocks * round) - 1)
	// add block `start - end` to dirty blocks map
	m.dirty[VirtAddr(blockStart)] = blockEnd
	return nil
}

// ReadIntoPerms reads data of `len(buf)` from memory into buf only if the region
// of memory been read has `perm` set on it
func (m Mmu) ReadIntoPerms(addr VirtAddr, buf []uint8, perm Perm) error {
	//get the permission on the region of memory to read from
	perms := m.permissions[int(addr) : len(buf)+int(addr)]

	for _, p := range perms {
		// check if all perms on region of memory is expected perm
		if (p & perm) != perm {
			return ErrMemIONotPermitted
		}
	}

	// copy from the address pointed to by `addr` to len(buf) into `buf`
	copy(buf, m.memory[int(addr):len(buf)+int(addr)])
	return nil
}

// ReadInto reads data of `len(buf)` from readable memory starting at addr into buf
func (m Mmu) ReadInto(addr VirtAddr, buf []uint8) error {
	return m.ReadIntoPerms(addr, buf, PERM_READ)
}

// Write `val` uint32 into writable memory
func (m *Mmu) WriteFrom32(addr VirtAddr, val uint32) error {
	buf := *(*[4]byte)(unsafe.Pointer(&val))
	return m.WriteFrom(addr, buf[:])
}

// Write 2-bytes to addr in memory
func (m *Mmu) WriteFrom16(addr VirtAddr, val uint16) error {
	buf := *(*[2]byte)(unsafe.Pointer(&val))
	return m.WriteFrom(addr, buf[:])
}

// Write 1-byte to addr in memory
func (m *Mmu) WriteFrom8(addr VirtAddr, val uint8) error {
	buf := *(*[1]byte)(unsafe.Pointer(&val))
	return m.WriteFrom(addr, buf[:])
}

// Read 4-bytes of memory starting at `addr` with permissions `perm`
func (m Mmu) ReadInto32(addr VirtAddr, perm Perm) (inst uint32, err error) {
	buf := make([]byte, 4)
	err = m.ReadIntoPerms(addr, buf, perm)
	if err == nil {
		inst = *(*uint32)(unsafe.Pointer(&buf[0]))
	}
	return
}

// Read 2-bytes of memory at addr
func (m Mmu) ReadInto16(addr VirtAddr, perm Perm) (inst uint16, err error) {
	buf := make([]byte, 2)
	err = m.ReadIntoPerms(addr, buf, perm)
	if err == nil {
		inst = *(*uint16)(unsafe.Pointer(&buf[0]))
	}
	return
}

// Read 1-byte of memory at addr
func (m Mmu) ReadInto8(addr VirtAddr, perm Perm) (inst uint8, err error) {
	buf := make([]byte, 1)
	err = m.ReadIntoPerms(addr, buf, perm)
	if err == nil {
		inst = *(*uint8)(unsafe.Pointer(&buf[0]))
	}
	return
}
