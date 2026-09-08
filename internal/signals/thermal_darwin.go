//go:build darwin && cgo

package signals

/*
#cgo CFLAGS: -x objective-c
#cgo LDFLAGS: -framework Foundation
#import <Foundation/Foundation.h>

// governorThermalState returns the raw NSProcessInfoThermalState (0..3), or -1 if the
// running OS predates the API. The @autoreleasepool matters: this is called at 1 Hz for
// the life of the daemon, and -processInfo hands back an autoreleased object.
//
// The value is read fresh on every call — NSProcessInfo does not cache it, and neither do
// we, because the control loop's whole job is to notice the moment it moves.
static int governorThermalState(void) {
	@autoreleasepool {
		NSProcessInfo *info = [NSProcessInfo processInfo];
		if (![info respondsToSelector:@selector(thermalState)]) {
			return -1;
		}
		return (int)[info thermalState];
	}
}
*/
import "C"

// readThermal reads ProcessInfo.thermalState. No privileges, no subprocess, no sudo.
func readThermal() Thermal {
	return thermalFromRaw(int(C.governorThermalState()))
}
