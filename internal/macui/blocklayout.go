package macui

import "unsafe"

// Objective-C blocks built by hand (the glue cannot use purego's objc
// package, whose init loads the runtime in every process that links it).
// The layout is Block_literal_1 of the Block ABI, for a block with no
// captured variables that lives for the whole process: a global block.
//
// The memory of a literal, its descriptor and its signature string is a
// package-level variable of the caller, allocated once and never moved or
// freed (Go does not move package-level data, and the C side keeps
// pointers to it). The invoke function is a purego callback, which cannot
// be freed either, so it is created once per block, never per call.

const (
	// blockIsGlobal marks a block the runtime never copies or frees
	// (BLOCK_IS_GLOBAL).
	blockIsGlobal = 1 << 28
	// blockHasSignature says the descriptor ends with a type encoding of the
	// block's signature (BLOCK_HAS_SIGNATURE); frameworks that forward a
	// completion handler may ask for it.
	blockHasSignature = 1 << 30

	// blockInvokeOffset is where the literal holds its invoke function
	// pointer, which a received block is called through, with the block as
	// the first argument.
	blockInvokeOffset = 16
	blockLiteralSize  = 32
	blockDescSize     = 24
)

// blockLiteral is Block_literal_1 on a 64-bit system.
type blockLiteral struct {
	isa        uintptr // _NSConcreteGlobalBlock
	flags      int32
	reserved   int32
	invoke     uintptr // void *(*)(block, args...)
	descriptor uintptr // *blockDescriptor
}

// blockDescriptor is Block_descriptor_1 followed by the signature, which
// sits there when the block has no copy and dispose helpers.
type blockDescriptor struct {
	reserved  uintptr
	size      uintptr
	signature *byte // NUL-terminated type encoding
}

// initGlobalBlock fills lit and desc as a global block of that isa that
// calls invoke, with a signature. All three must outlive the process's use
// of the block.
func initGlobalBlock(lit *blockLiteral, desc *blockDescriptor, isa, invoke uintptr, signature *byte) {
	*desc = blockDescriptor{size: blockLiteralSize, signature: signature}
	*lit = blockLiteral{
		isa:        isa,
		flags:      blockIsGlobal | blockHasSignature,
		invoke:     invoke,
		descriptor: uintptr(unsafe.Pointer(desc)),
	}
}

// readInvoke is the invoke function pointer of the block at address b, 0
// for nil. The address is the system's (a block it handed us), so it is
// read through a pointer made without the conversion go vet flags.
func readInvoke(b uintptr) uintptr {
	if b == 0 {
		return 0
	}
	p := *(*unsafe.Pointer)(unsafe.Pointer(&b))
	return *(*uintptr)(unsafe.Add(p, blockInvokeOffset))
}
