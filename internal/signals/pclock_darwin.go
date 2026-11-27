//go:build darwin && cgo

package signals

/*
#cgo LDFLAGS: -framework CoreFoundation -framework IOKit
#include <CoreFoundation/CoreFoundation.h>
#include <IOKit/IOKitLib.h>
#include <dlfcn.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>

// IOReport is private API. It is resolved with dlopen/dlsym at run time and NEVER linked, so a
// macOS that drops or renames it makes the P-cluster clock unknown instead of making the
// governor fail to launch. CoreFoundation and IOKit are public frameworks.
//
// This file only fetches, as text. pclock.go interprets, where each failure is testable.

enum { gpcOK = 0, gpcNoLib = -1, gpcNoSym = -2, gpcNoChannels = -3, gpcNoSub = -4, gpcNoSample = -5, gpcShort = -6, gpcNoPmgr = -7 };

typedef CFDictionaryRef (*gpcCopyChannels)(CFStringRef, CFStringRef, uint64_t, uint64_t, uint64_t);
typedef void *(*gpcCreateSub)(void *, CFMutableDictionaryRef, CFMutableDictionaryRef *, uint64_t, CFTypeRef);
typedef CFDictionaryRef (*gpcCreateSamples)(void *, CFMutableDictionaryRef, CFTypeRef);
typedef CFStringRef (*gpcGetStr)(CFDictionaryRef);
typedef int32_t (*gpcStateCount)(CFDictionaryRef);
typedef CFStringRef (*gpcStateName)(CFDictionaryRef, int32_t);
typedef int64_t (*gpcStateRes)(CFDictionaryRef, int32_t);

static gpcCreateSamples gpcSamples;
static gpcGetStr gpcSubGroup, gpcName;
static gpcStateCount gpcCount;
static gpcStateName gpcSName;
static gpcStateRes gpcSRes;
static void *gpcSub;
static CFMutableDictionaryRef gpcSubbed;

static void gpcStr(CFStringRef s, char *b, size_t n) {
	b[0] = 0;
	if (s) CFStringGetCString(s, b, (CFIndex)n, kCFStringEncodingUTF8);
}

// gpcOpen subscribes to the CPU complexes' performance-state residency, once per process.
static int gpcOpen(void) {
	void *h = dlopen("/usr/lib/libIOReport.dylib", RTLD_NOW | RTLD_LOCAL);
	if (!h) return gpcNoLib;
	gpcCopyChannels copy = (gpcCopyChannels)dlsym(h, "IOReportCopyChannelsInGroup");
	gpcCreateSub sub = (gpcCreateSub)dlsym(h, "IOReportCreateSubscription");
	gpcSamples = (gpcCreateSamples)dlsym(h, "IOReportCreateSamples");
	gpcSubGroup = (gpcGetStr)dlsym(h, "IOReportChannelGetSubGroup");
	gpcName = (gpcGetStr)dlsym(h, "IOReportChannelGetChannelName");
	gpcCount = (gpcStateCount)dlsym(h, "IOReportStateGetCount");
	gpcSName = (gpcStateName)dlsym(h, "IOReportStateGetNameForIndex");
	gpcSRes = (gpcStateRes)dlsym(h, "IOReportStateGetResidency");
	if (!copy || !sub || !gpcSamples || !gpcSubGroup || !gpcName || !gpcCount || !gpcSName || !gpcSRes) return gpcNoSym;
	CFDictionaryRef ch = copy(CFSTR("CPU Stats"), CFSTR("CPU Complex Performance States"), 0, 0, 0);
	if (!ch) return gpcNoChannels;
	CFMutableDictionaryRef want = CFDictionaryCreateMutableCopy(NULL, 0, ch);
	CFRelease(ch);
	gpcSub = sub(NULL, want, &gpcSubbed, 0, NULL);
	CFRelease(want);
	if (!gpcSub || !gpcSubbed) return gpcNoSub;
	return gpcOK;
}

// gpcSample writes one line per CPU complex: name, then TAB-separated state=cumulative ticks.
static int gpcSample(char *buf, int cap) {
	CFDictionaryRef s = gpcSamples(gpcSub, gpcSubbed, NULL);
	if (!s) return gpcNoSample;
	CFArrayRef arr = CFDictionaryGetValue(s, CFSTR("IOReportChannels"));
	int n = 0;
	char a[128], b[64];
	for (CFIndex i = 0; arr && i < CFArrayGetCount(arr); i++) {
		CFDictionaryRef c = CFArrayGetValueAtIndex(arr, i);
		gpcStr(gpcSubGroup(c), a, sizeof a);
		if (strcmp(a, "CPU Complex Performance States") != 0) continue;
		gpcStr(gpcName(c), a, sizeof a);
		n += snprintf(buf + n, cap > n ? cap - n : 0, "%s", a);
		int32_t k = gpcCount(c);
		for (int32_t j = 0; j < k; j++) {
			gpcStr(gpcSName(c, j), b, sizeof b);
			n += snprintf(buf + n, cap > n ? cap - n : 0, "\t%s=%lld", b, (long long)gpcSRes(c, j));
		}
		n += snprintf(buf + n, cap > n ? cap - n : 0, "\n");
	}
	CFRelease(s);
	return n < cap ? n : gpcShort;
}

// gpcTables writes the power manager's voltage-state tables: property name, TAB, hex bytes.
static int gpcTables(char *buf, int cap) {
	io_service_t pm = IOServiceGetMatchingService(0, IOServiceNameMatching("pmgr"));
	if (!pm) return gpcNoPmgr;
	CFMutableDictionaryRef props = NULL;
	kern_return_t kr = IORegistryEntryCreateCFProperties(pm, &props, kCFAllocatorDefault, 0);
	IOObjectRelease(pm);
	if (kr != KERN_SUCCESS || !props) return gpcNoPmgr;
	CFIndex cnt = CFDictionaryGetCount(props);
	const void **keys = malloc(sizeof(void *) * cnt), **vals = malloc(sizeof(void *) * cnt);
	CFDictionaryGetKeysAndValues(props, keys, vals);
	int n = 0;
	char name[128];
	for (CFIndex i = 0; i < cnt; i++) {
		if (CFGetTypeID(keys[i]) != CFStringGetTypeID() || CFGetTypeID(vals[i]) != CFDataGetTypeID()) continue;
		gpcStr(keys[i], name, sizeof name);
		if (strncmp(name, "voltage-states", 14) != 0) continue;
		n += snprintf(buf + n, cap > n ? cap - n : 0, "%s\t", name);
		const UInt8 *p = CFDataGetBytePtr(vals[i]);
		for (CFIndex j = 0; j < CFDataGetLength(vals[i]); j++) {
			n += snprintf(buf + n, cap > n ? cap - n : 0, "%02x", p[j]);
		}
		n += snprintf(buf + n, cap > n ? cap - n : 0, "\n");
	}
	free(keys);
	free(vals);
	CFRelease(props);
	return n < cap ? n : gpcShort;
}
*/
import "C"

import (
	"fmt"
	"sync"
	"unsafe"
)

var pclockFailures = map[C.int]string{
	C.gpcNoLib:      "libIOReport.dylib did not load",
	C.gpcNoSym:      "an IOReport symbol is missing",
	C.gpcNoChannels: "no CPU performance-state channels",
	C.gpcNoSub:      "IOReport subscription refused",
	C.gpcNoSample:   "IOReport sample returned nothing",
	C.gpcShort:      "IOReport output exceeded the buffer",
	C.gpcNoPmgr:     "no pmgr entry in the IORegistry",
}

func pclockFail(rc C.int) error {
	if s, ok := pclockFailures[rc]; ok {
		return pclockFailure(s)
	}
	return pclockFailure(fmt.Sprintf("IOReport code %d", int(rc)))
}

// The C side holds one subscription in statics, so it is opened once per process however many
// samplers are constructed (tests build many).
var (
	pclockOnce    sync.Once
	pclockOpenErr error
)

type iorSource struct{ buf []byte }

func newPClockSource() (pclockSource, error) {
	pclockOnce.Do(func() {
		if rc := C.gpcOpen(); rc != C.gpcOK {
			pclockOpenErr = pclockFail(rc)
		}
	})
	if pclockOpenErr != nil {
		return nil, pclockOpenErr
	}
	return &iorSource{buf: make([]byte, 1<<16)}, nil
}

func (s *iorSource) read(f func(*C.char, C.int) C.int) (string, error) {
	rc := f((*C.char)(unsafe.Pointer(&s.buf[0])), C.int(len(s.buf)))
	if rc < 0 {
		return "", pclockFail(rc)
	}
	return string(s.buf[:rc]), nil
}

func (s *iorSource) Sample() (string, error) {
	return s.read(func(b *C.char, n C.int) C.int { return C.gpcSample(b, n) })
}

func (s *iorSource) Tables() (string, error) {
	return s.read(func(b *C.char, n C.int) C.int { return C.gpcTables(b, n) })
}
