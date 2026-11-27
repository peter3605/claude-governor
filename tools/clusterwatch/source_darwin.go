//go:build darwin && cgo

package main

/*
#cgo LDFLAGS: -framework CoreFoundation -framework IOKit
#include <CoreFoundation/CoreFoundation.h>
#include <IOKit/IOKitLib.h>
#include <dlfcn.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>

// Everything private is resolved with dlopen/dlsym at run time and never linked, so a macOS
// that removes or renames a symbol makes this tool report "unknown" instead of refusing to
// launch. CoreFoundation and IOKit themselves are public frameworks.
//
// This file only FETCHES: each function renders what the OS returned as plain text, and every
// interpretation (which channel is the P-cluster, which table gives its frequencies, what a
// delta means) is pure Go in parse.go, where each failure shape is testable without the
// frameworks.

enum { cwOK = 0, cwNoLib = -1, cwNoSym = -2, cwNoChannels = -3, cwNoSub = -4, cwNoSample = -5, cwShort = -6, cwNoPmgr = -7 };

typedef CFDictionaryRef (*cwCopyChannels)(CFStringRef, CFStringRef, uint64_t, uint64_t, uint64_t);
typedef void *(*cwCreateSub)(void *, CFMutableDictionaryRef, CFMutableDictionaryRef *, uint64_t, CFTypeRef);
typedef CFDictionaryRef (*cwCreateSamples)(void *, CFMutableDictionaryRef, CFTypeRef);
typedef CFStringRef (*cwGetStr)(CFDictionaryRef);
typedef int32_t (*cwStateCount)(CFDictionaryRef);
typedef CFStringRef (*cwStateName)(CFDictionaryRef, int32_t);
typedef int64_t (*cwStateRes)(CFDictionaryRef, int32_t);

static cwCreateSamples cwSamples;
static cwGetStr cwSubGroup, cwName;
static cwStateCount cwCount;
static cwStateName cwSName;
static cwStateRes cwSRes;
static void *cwSub;
static CFMutableDictionaryRef cwSubbed;

static void cwStr(CFStringRef s, char *b, size_t n) {
	b[0] = 0;
	if (s) CFStringGetCString(s, b, (CFIndex)n, kCFStringEncodingUTF8);
}

// cwOpen subscribes to the CPU complexes' performance-state residency: per cluster, how many
// 24 MHz ticks it spent idle and in each DVFS state. Counted by the hardware whatever thread is
// running where, which is what clockprobe could not do.
static int cwOpen(void) {
	void *h = dlopen("/usr/lib/libIOReport.dylib", RTLD_NOW | RTLD_LOCAL);
	if (!h) return cwNoLib;
	cwCopyChannels copy = (cwCopyChannels)dlsym(h, "IOReportCopyChannelsInGroup");
	cwCreateSub sub = (cwCreateSub)dlsym(h, "IOReportCreateSubscription");
	cwSamples = (cwCreateSamples)dlsym(h, "IOReportCreateSamples");
	cwSubGroup = (cwGetStr)dlsym(h, "IOReportChannelGetSubGroup");
	cwName = (cwGetStr)dlsym(h, "IOReportChannelGetChannelName");
	cwCount = (cwStateCount)dlsym(h, "IOReportStateGetCount");
	cwSName = (cwStateName)dlsym(h, "IOReportStateGetNameForIndex");
	cwSRes = (cwStateRes)dlsym(h, "IOReportStateGetResidency");
	if (!copy || !sub || !cwSamples || !cwSubGroup || !cwName || !cwCount || !cwSName || !cwSRes) return cwNoSym;
	CFDictionaryRef ch = copy(CFSTR("CPU Stats"), CFSTR("CPU Complex Performance States"), 0, 0, 0);
	if (!ch) return cwNoChannels;
	CFMutableDictionaryRef want = CFDictionaryCreateMutableCopy(NULL, 0, ch);
	CFRelease(ch);
	cwSub = sub(NULL, want, &cwSubbed, 0, NULL);
	CFRelease(want);
	if (!cwSub || !cwSubbed) return cwNoSub;
	return cwOK;
}

// cwSample writes one line per channel: name, then TAB-separated state=residency pairs, the
// counters cumulative (parse.go takes the delta between two samples).
static int cwSample(char *buf, int cap) {
	CFDictionaryRef s = cwSamples(cwSub, cwSubbed, NULL);
	if (!s) return cwNoSample;
	CFArrayRef arr = CFDictionaryGetValue(s, CFSTR("IOReportChannels"));
	int n = 0;
	char a[128], b[64];
	for (CFIndex i = 0; arr && i < CFArrayGetCount(arr); i++) {
		CFDictionaryRef c = CFArrayGetValueAtIndex(arr, i);
		cwStr(cwSubGroup(c), a, sizeof a);
		if (strcmp(a, "CPU Complex Performance States") != 0) continue;
		cwStr(cwName(c), a, sizeof a);
		n += snprintf(buf + n, cap > n ? cap - n : 0, "%s", a);
		int32_t k = cwCount(c);
		for (int32_t j = 0; j < k; j++) {
			cwStr(cwSName(c, j), b, sizeof b);
			n += snprintf(buf + n, cap > n ? cap - n : 0, "\t%s=%lld", b, (long long)cwSRes(c, j));
		}
		n += snprintf(buf + n, cap > n ? cap - n : 0, "\n");
	}
	CFRelease(s);
	return n < cap ? n : cwShort;
}

// cwTables writes the power manager's voltage-state tables, one per line: property name, TAB,
// the raw bytes in hex. They are (frequency Hz, voltage) pairs of little-endian uint32s;
// which table belongs to which cluster is decided in Go, by matching state counts.
static int cwTables(char *buf, int cap) {
	io_service_t pm = IOServiceGetMatchingService(0, IOServiceNameMatching("pmgr"));
	if (!pm) return cwNoPmgr;
	CFMutableDictionaryRef props = NULL;
	kern_return_t kr = IORegistryEntryCreateCFProperties(pm, &props, kCFAllocatorDefault, 0);
	IOObjectRelease(pm);
	if (kr != KERN_SUCCESS || !props) return cwNoPmgr;
	CFIndex cnt = CFDictionaryGetCount(props);
	const void **keys = malloc(sizeof(void *) * cnt), **vals = malloc(sizeof(void *) * cnt);
	CFDictionaryGetKeysAndValues(props, keys, vals);
	int n = 0;
	char name[128];
	for (CFIndex i = 0; i < cnt; i++) {
		if (CFGetTypeID(keys[i]) != CFStringGetTypeID() || CFGetTypeID(vals[i]) != CFDataGetTypeID()) continue;
		cwStr(keys[i], name, sizeof name);
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
	return n < cap ? n : cwShort;
}

// IOHID's event-system client is private API inside IOKit; resolved by name from the IOKit
// image already loaded, never linked.
typedef void *(*cwHIDCreate)(CFAllocatorRef);
typedef int (*cwHIDMatch)(void *, CFDictionaryRef);
typedef CFArrayRef (*cwHIDServices)(void *);
typedef CFTypeRef (*cwHIDProp)(void *, CFStringRef);
typedef void *(*cwHIDEvent)(void *, int64_t, int32_t, int64_t);
typedef double (*cwHIDFloat)(void *, int32_t);

static cwHIDServices cwServices;
static cwHIDProp cwProp;
static cwHIDEvent cwEvent;
static cwHIDFloat cwFloat;
static void *cwHID;

enum { cwTempType = 15 }; // kIOHIDEventTypeTemperature

// cwKept holds the sensors whose name starts with the prefix Go passed, found once at start-up,
// with their names. Walking every sensor and copying its name each tick cost 0.4% of a core.
static CFMutableArrayRef cwKept, cwKeptNames;

static int cwOpenHID(const char *prefix) {
	cwHIDCreate create = (cwHIDCreate)dlsym(RTLD_DEFAULT, "IOHIDEventSystemClientCreate");
	cwHIDMatch match = (cwHIDMatch)dlsym(RTLD_DEFAULT, "IOHIDEventSystemClientSetMatching");
	cwServices = (cwHIDServices)dlsym(RTLD_DEFAULT, "IOHIDEventSystemClientCopyServices");
	cwProp = (cwHIDProp)dlsym(RTLD_DEFAULT, "IOHIDServiceClientCopyProperty");
	cwEvent = (cwHIDEvent)dlsym(RTLD_DEFAULT, "IOHIDServiceClientCopyEvent");
	cwFloat = (cwHIDFloat)dlsym(RTLD_DEFAULT, "IOHIDEventGetFloatValue");
	if (!create || !match || !cwServices || !cwProp || !cwEvent || !cwFloat) return cwNoSym;
	cwHID = create(kCFAllocatorDefault);
	if (!cwHID) return cwNoSub;
	int page = 0xff00, usage = 5; // Apple vendor page, temperature sensor
	CFNumberRef p = CFNumberCreate(NULL, kCFNumberIntType, &page), u = CFNumberCreate(NULL, kCFNumberIntType, &usage);
	const void *k[] = {CFSTR("PrimaryUsagePage"), CFSTR("PrimaryUsage")}, *v[] = {p, u};
	CFDictionaryRef m = CFDictionaryCreate(NULL, k, v, 2, &kCFTypeDictionaryKeyCallBacks, &kCFTypeDictionaryValueCallBacks);
	match(cwHID, m);
	CFRelease(m);
	CFRelease(p);
	CFRelease(u);
	CFArrayRef s = cwServices(cwHID);
	if (!s) return cwNoSample;
	cwKept = CFArrayCreateMutable(NULL, 0, &kCFTypeArrayCallBacks);
	cwKeptNames = CFArrayCreateMutable(NULL, 0, &kCFTypeArrayCallBacks);
	char name[128];
	for (CFIndex i = 0; i < CFArrayGetCount(s); i++) {
		const void *sv = CFArrayGetValueAtIndex(s, i);
		CFTypeRef pn = cwProp((void *)sv, CFSTR("Product"));
		if (!pn) continue;
		if (CFGetTypeID(pn) == CFStringGetTypeID()) {
			cwStr(pn, name, sizeof name);
			if (strncmp(name, prefix, strlen(prefix)) == 0) {
				CFArrayAppendValue(cwKept, sv);
				CFArrayAppendValue(cwKeptNames, pn);
			}
		}
		CFRelease(pn);
	}
	CFRelease(s);
	return cwOK;
}

// cwTemps writes one line per kept sensor: name, TAB, degrees C.
static int cwTemps(char *buf, int cap) {
	int n = 0;
	char name[128];
	for (CFIndex i = 0; i < CFArrayGetCount(cwKept); i++) {
		void *e = cwEvent((void *)CFArrayGetValueAtIndex(cwKept, i), cwTempType, 0, 0);
		if (!e) continue;
		cwStr(CFArrayGetValueAtIndex(cwKeptNames, i), name, sizeof name);
		n += snprintf(buf + n, cap > n ? cap - n : 0, "%s\t%.2f\n", name, cwFloat(e, cwTempType << 16));
		CFRelease(e);
	}
	return n < cap ? n : cwShort;
}
*/
import "C"

import (
	"fmt"
	"unsafe"
)

const supported = true

var failures = map[C.int]string{
	C.cwNoLib:      "libIOReport.dylib did not load",
	C.cwNoSym:      "a private symbol is missing",
	C.cwNoChannels: "no CPU performance-state channels",
	C.cwNoSub:      "subscription refused",
	C.cwNoSample:   "sample returned nothing",
	C.cwShort:      "buffer too small",
	C.cwNoPmgr:     "no pmgr entry in the IORegistry",
}

func fail(rc C.int) error {
	if s, ok := failures[rc]; ok {
		return failure(s)
	}
	return failure(fmt.Sprintf("code %d", int(rc)))
}

const bufSize = 1 << 17

// osSource is the real source. Its methods return text or an error, never a guess.
type osSource struct {
	buf    []byte
	hidErr error
}

func newOSSource() (source, error) {
	s := &osSource{buf: make([]byte, bufSize)}
	if rc := C.cwOpen(); rc != C.cwOK {
		return nil, fail(rc)
	}
	prefix := C.CString(pSensorPrefix)
	defer C.free(unsafe.Pointer(prefix))
	if rc := C.cwOpenHID(prefix); rc != C.cwOK {
		s.hidErr = fail(rc) // temperatures alone are lost; the frequency reading stands
	}
	return s, nil
}

func (s *osSource) read(f func(*C.char, C.int) C.int) (string, error) {
	rc := f((*C.char)(unsafe.Pointer(&s.buf[0])), C.int(len(s.buf)))
	if rc < 0 {
		return "", fail(rc)
	}
	return string(s.buf[:rc]), nil
}

func (s *osSource) Sample() (string, error) {
	return s.read(func(b *C.char, n C.int) C.int { return C.cwSample(b, n) })
}

func (s *osSource) Tables() (string, error) {
	return s.read(func(b *C.char, n C.int) C.int { return C.cwTables(b, n) })
}

func (s *osSource) Temps() (string, error) {
	if s.hidErr != nil {
		return "", s.hidErr
	}
	return s.read(func(b *C.char, n C.int) C.int { return C.cwTemps(b, n) })
}
