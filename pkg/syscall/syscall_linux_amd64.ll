; Copyright 2012 Andrew Wilkins.
; Use of this source code is governed by an MIT-style
; license that can be found in the LICENSE file.
;
; Defines low-level Syscall functions.

target datalayout = "e-p:64:64:64-S128-i1:8:8-i8:8:8-i16:16:16-i32:32:32-i64:64:64-f16:16:16-f32:32:32-f64:64:64-f128:128:128-v64:64:64-v128:128:128-a0:0:64-s0:64:64-f80:128:128-n8:16:32:64"
target triple = "x86_64-unknown-linux"

; r1, r2, errno
%syscallres = type {i64, i64, i64}

define %syscallres @syscall.RawSyscall(i64, i64, i64, i64) {
entry:
	%4 = call {i64, i64} asm sideeffect "syscall\0A", "={ax},={dx},{ax},{di},{si},{dx},{r10},{r8},{r9}"(i64 %0, i64 %1, i64 %2, i64 %3, i64 0, i64 0, i64 0) nounwind
	%5 = extractvalue {i64, i64} %4, 0
	%6 = extractvalue {i64, i64} %4, 1
	%7 = insertvalue %syscallres undef, i64 %5, 0
	%8 = insertvalue %syscallres %7, i64 %6, 1
	%9 = insertvalue %syscallres %8, i64 0, 2
	; TODO check result, update results appropriately.
	ret %syscallres %9
}

; No tie-in with runtime yet, since there's no scheduler. Just alias it.
@syscall.Syscall = alias %syscallres (i64, i64, i64, i64)* @syscall.RawSyscall

