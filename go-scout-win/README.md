# Restored Windows source

This directory preserves the restored `go-scout-win` project and its vendored
source dependencies, including its custom ROS hostname resolution patch. Local
robot addresses have been replaced with loopback examples. Original Git history,
executables and the camera demo are excluded.

This is the older implementation, retained for reference. It predates the current
stop/lease, cancellation, camera and agent API fixes. Use the maintained application
at the [repository root](../README.md) for controlling Scout on Windows or macOS;
it builds `scout.exe` on Windows and includes the current Xbox/keyboard controls.

To compile this historical version for inspection from Windows PowerShell:

```powershell
go build -mod=vendor -o ..\dist\scout-legacy-windows.exe .
```

The dependency licenses are retained in `vendor/`. This copy is not used by the
root module or the current release packages.
