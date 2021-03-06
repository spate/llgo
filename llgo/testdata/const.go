package main

import "runtime"

const (
	a = iota * 2
	A = 1
	B
	C
	D = Z + iota
)

const (
	Z    = iota
	Big  = 1<<31 - 1
	Big2 = -2147483648
	Big3 = 2147483647
)

const (
	expbits32 uint = 8
	bias32         = -1<<(expbits32-1) + 1
	darwinAMD64 = runtime.GOOS == "darwin" && runtime.GOARCH == "amd64"
)

func main() {
	println(a)
	println(B)
	println(A, A)
	println(A, B, C, D)
	println(Big)
	println(Big2)
	println(Big3)
	println(bias32)

	// Currently fails, due to difference in C printf and Go's println
	// formatting of the exponent.
	//println(10 * 1e9)
	println(darwinAMD64)
}
