# Wails v3 Android Example

This example runs on Android (emulator and device) as well as desktop. See
[`ANDROID.md`](../../ANDROID.md) for the full Android guide.

```bash
wails3 dev --target android                                  # build + launch in the Android Emulator
wails3 package --target android/arm64 --format apk           # production release APK
wails3 android logs                                         # stream logcat output
```

It demonstrates service bindings, Go->JS events, native AlertDialog message
dialogs, clipboard, device info, and screen metrics.
