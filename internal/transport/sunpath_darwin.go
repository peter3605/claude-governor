package transport

// MaxSocketPath is the longest usable unix socket path on this platform.
//
// sockaddr_un.sun_path is 104 bytes on darwin and 108 on Linux, including the terminator.
// Exceeding it is worth naming rather than passing through, because the kernel's answer is
// EINVAL — reported as "bind: invalid argument" or "connect: invalid argument", which reads
// like a bad argument to Listen rather than like a path that is four characters too long.
// This repo has already paid for that confusion once, in the test helper that skips a test
// whose temp directory pushes past it.
const MaxSocketPath = 103
