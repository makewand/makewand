"""Small flock-compatible lock API for the stdlib-only bundled engine."""
try:
    from fcntl import LOCK_EX, LOCK_NB, LOCK_SH, LOCK_UN, flock
except ImportError:  # Windows: lock one shared byte without changing file offsets.
    import ctypes
    from ctypes import wintypes
    import msvcrt

    LOCK_SH, LOCK_EX, LOCK_NB, LOCK_UN = 1, 2, 4, 8

    class _Overlapped(ctypes.Structure):
        _fields_ = [("Internal", ctypes.c_size_t), ("InternalHigh", ctypes.c_size_t),
                    ("Offset", wintypes.DWORD), ("OffsetHigh", wintypes.DWORD),
                    ("hEvent", wintypes.HANDLE)]

    _kernel32 = ctypes.WinDLL("kernel32", use_last_error=True)
    _lock = _kernel32.LockFileEx
    _lock.argtypes = [wintypes.HANDLE, wintypes.DWORD, wintypes.DWORD,
                      wintypes.DWORD, wintypes.DWORD, ctypes.POINTER(_Overlapped)]
    _lock.restype = wintypes.BOOL
    _unlock = _kernel32.UnlockFileEx
    _unlock.argtypes = [wintypes.HANDLE, wintypes.DWORD, wintypes.DWORD,
                        wintypes.DWORD, ctypes.POINTER(_Overlapped)]
    _unlock.restype = wintypes.BOOL

    def flock(file, operation):
        descriptor = file if isinstance(file, int) else file.fileno()
        handle = msvcrt.get_osfhandle(descriptor)
        overlapped = _Overlapped()
        if operation & LOCK_UN:
            succeeded = _unlock(handle, 0, 1, 0, ctypes.byref(overlapped))
        else:
            flags = (2 if operation & LOCK_EX else 0) | (1 if operation & LOCK_NB else 0)
            succeeded = _lock(handle, flags, 0, 1, 0, ctypes.byref(overlapped))
        if not succeeded:
            raise ctypes.WinError(ctypes.get_last_error())
