"""Native Windows file operations with pinned, non-reparse directory handles.

Windows does not implement POSIX dir_fd. Keeping every ancestor open without
FILE_SHARE_DELETE prevents rename/replacement while an absolute-path operation
is in progress. Leaf reads additionally deny shared writes. No junction, other
reparse point, alternate stream, DOS device name or ambiguous name is accepted.
"""
import contextlib
import base64
import ctypes
import hashlib
import ntpath
import os
import re
import stat
import uuid
from pathlib import Path


def relative_parts(value):
    if not isinstance(value, str) or not value or "\\" in value or "\0" in value:
        raise ValueError("Windows workspace paths must use unambiguous relative '/' names")
    parts = value.split("/")
    reserved = re.compile(r"^(?:CON|PRN|AUX|NUL|COM[1-9¹²³]|LPT[1-9¹²³])(?:\..*)?$", re.I)
    if any(not part or part in (".", "..") or part.casefold() == ".git"
           or part[-1:] in (" ", ".") or any(ord(char) < 32 or char in '<>:"|?*' for char in part)
           or reserved.fullmatch(part) for part in parts):
        raise ValueError("Unsafe or ambiguous Windows workspace path: " + repr(value))
    return parts


def _api():
    if os.name != "nt":
        raise OSError("Native Windows handles are unavailable on this platform")
    from ctypes import wintypes
    kernel = ctypes.WinDLL("kernel32", use_last_error=True)
    kernel.CreateFileW.argtypes = [wintypes.LPCWSTR, wintypes.DWORD, wintypes.DWORD,
                                  ctypes.c_void_p, wintypes.DWORD, wintypes.DWORD, wintypes.HANDLE]
    kernel.CreateFileW.restype = wintypes.HANDLE
    kernel.CloseHandle.argtypes = [wintypes.HANDLE]
    kernel.CloseHandle.restype = wintypes.BOOL
    return kernel


def _private_state_sddl(user_sid, *, directory=False):
    """Grant this user and SYSTEM once each, including a SYSTEM caller."""
    flags = "OICI" if directory else ""
    principals = ("SY",) if user_sid == "S-1-5-18" else (user_sid, "SY")
    return "D:P" + "".join("(A;" + flags + ";FA;;;" + sid + ")" for sid in principals)


def _security_descriptor(path, information):
    from ctypes import wintypes
    advapi = ctypes.WinDLL("advapi32", use_last_error=True)
    get = advapi.GetFileSecurityW
    get.argtypes = [wintypes.LPCWSTR, wintypes.DWORD, ctypes.c_void_p, wintypes.DWORD, ctypes.POINTER(wintypes.DWORD)]
    get.restype = wintypes.BOOL
    needed = wintypes.DWORD()
    api_path = _api_path(path)
    get(api_path, information, None, 0, ctypes.byref(needed))
    if not needed.value:
        raise ctypes.WinError(ctypes.get_last_error())
    descriptor = ctypes.create_string_buffer(needed.value)
    if not get(api_path, information, descriptor, needed.value, ctypes.byref(needed)):
        raise ctypes.WinError(ctypes.get_last_error())
    return descriptor


def _set_dacl(path, descriptor, *, protected=None):
    """Set the original DACL on a captured object without re-inheriting ACEs."""
    from ctypes import wintypes
    path = Path(path)
    advapi = ctypes.WinDLL("advapi32", use_last_error=True)
    # The NT query/set pair uses the descriptor actually stored on the file.
    # GetSecurityInfo can present legacy ACEs through the current inheritance
    # model, and SetSecurityInfo propagates/reconstructs inherited ACEs. Neither
    # behavior is suitable for restoring an exact sealed preimage.
    # https://learn.microsoft.com/en-us/windows-hardware/drivers/ddi/ntifs/nf-ntifs-zwsetsecurityobject
    length = advapi.GetSecurityDescriptorLength
    length.argtypes = [ctypes.c_void_p]
    length.restype = wintypes.DWORD
    size = length(descriptor)
    if not 20 <= size <= 65536:
        raise ValueError("Windows application security descriptor exceeds its bound")
    owned = ctypes.create_string_buffer(ctypes.string_at(descriptor, size))
    if protected is not None:
        set_control = advapi.SetSecurityDescriptorControl
        set_control.argtypes = [ctypes.c_void_p, wintypes.WORD, wintypes.WORD]
        set_control.restype = wintypes.BOOL
        if not set_control(owned, 0x1000, 0x1000 if protected else 0):
            raise ctypes.WinError(ctypes.get_last_error())
    with pinned_directory(path.parent):
        handle = _open(path, directory=os.path.isdir(_api_path(path)), access=0x40000 | 0x20000 | 0x80)
        try:
            ntdll = ctypes.WinDLL("ntdll", use_last_error=True)
            setter = ntdll.NtSetSecurityObject
            setter.argtypes = [wintypes.HANDLE, wintypes.DWORD, ctypes.c_void_p]
            setter.restype = ctypes.c_int32
            status = setter(handle, 4, owned)  # DACL_SECURITY_INFORMATION only
            _check_nt_status(status)
            if (_application_security_value(_open_file_security_descriptor(handle))
                    != _application_security_value(owned)):
                raise ValueError("Windows did not preserve the exact original application DACL")
        finally:
            _api().CloseHandle(handle)


def _open_file_security_descriptor(handle):
    """Copy the DACL descriptor of one captured object, never a fresh pathname."""
    from ctypes import wintypes
    advapi = ctypes.WinDLL("advapi32", use_last_error=True)
    ntdll = ctypes.WinDLL("ntdll", use_last_error=True)
    getter = ntdll.NtQuerySecurityObject
    getter.argtypes = [wintypes.HANDLE, wintypes.DWORD, ctypes.c_void_p, wintypes.ULONG,
                      ctypes.POINTER(wintypes.ULONG)]
    getter.restype = ctypes.c_int32
    needed = wintypes.ULONG()
    # NTFS limits descriptors to 64 KiB; one bounded buffer avoids a size/query
    # race. Unlike GetSecurityInfo this reads the saved ACE flags without
    # converting a legacy inheritance model for presentation.
    # https://learn.microsoft.com/en-us/windows-hardware/drivers/ddi/ntifs/nf-ntifs-zwquerysecurityobject
    descriptor = ctypes.create_string_buffer(65536)
    status = getter(handle, 4, descriptor, len(descriptor), ctypes.byref(needed))
    _check_nt_status(status)
    if not 20 <= needed.value <= len(descriptor):
        raise ValueError("Windows application security descriptor exceeds its bound")
    # Construct a canonical DACL-only self-relative snapshot while the
    # original descriptor and its ACL pointers are still alive.
    class AbsoluteDescriptor(ctypes.Structure):
        _fields_ = [("revision", ctypes.c_ubyte), ("reserved", ctypes.c_ubyte),
                    ("control", wintypes.WORD), ("owner", ctypes.c_void_p),
                    ("group", ctypes.c_void_p), ("sacl", ctypes.c_void_p), ("dacl", ctypes.c_void_p)]
    signatures = {
        "GetSecurityDescriptorDacl": ([ctypes.c_void_p, ctypes.POINTER(wintypes.BOOL), ctypes.POINTER(ctypes.c_void_p), ctypes.POINTER(wintypes.BOOL)], wintypes.BOOL),
        "GetSecurityDescriptorControl": ([ctypes.c_void_p, ctypes.POINTER(wintypes.WORD), ctypes.POINTER(wintypes.DWORD)], wintypes.BOOL),
        "InitializeSecurityDescriptor": ([ctypes.c_void_p, wintypes.DWORD], wintypes.BOOL),
        "SetSecurityDescriptorDacl": ([ctypes.c_void_p, wintypes.BOOL, ctypes.c_void_p, wintypes.BOOL], wintypes.BOOL),
        "SetSecurityDescriptorControl": ([ctypes.c_void_p, wintypes.WORD, wintypes.WORD], wintypes.BOOL),
        "MakeSelfRelativeSD": ([ctypes.c_void_p, ctypes.c_void_p, ctypes.POINTER(wintypes.DWORD)], wintypes.BOOL),
    }
    for name, (arguments, result) in signatures.items():
        getattr(advapi, name).argtypes = arguments
        getattr(advapi, name).restype = result
    present, defaulted, acl = wintypes.BOOL(), wintypes.BOOL(), ctypes.c_void_p()
    control, revision = wintypes.WORD(), wintypes.DWORD()
    absolute = AbsoluteDescriptor()
    if (not advapi.GetSecurityDescriptorDacl(descriptor, ctypes.byref(present), ctypes.byref(acl), ctypes.byref(defaulted))
            or not advapi.GetSecurityDescriptorControl(descriptor, ctypes.byref(control), ctypes.byref(revision))
            or not advapi.InitializeSecurityDescriptor(ctypes.byref(absolute), 1)
            or not advapi.SetSecurityDescriptorDacl(ctypes.byref(absolute), present, acl, defaulted)
            or not advapi.SetSecurityDescriptorControl(ctypes.byref(absolute), 0x1000, control.value & 0x1000)):
        raise ctypes.WinError(ctypes.get_last_error())
    size = wintypes.DWORD()
    advapi.MakeSelfRelativeSD(ctypes.byref(absolute), None, ctypes.byref(size))
    if not 20 <= size.value <= 65536:
        raise ValueError("Windows application security descriptor exceeds its bound")
    snapshot = ctypes.create_string_buffer(size.value)
    if not advapi.MakeSelfRelativeSD(ctypes.byref(absolute), snapshot, ctypes.byref(size)):
        raise ctypes.WinError(ctypes.get_last_error())
    return ctypes.create_string_buffer(snapshot.raw)


def _application_security_value(descriptor):
    from ctypes import wintypes
    advapi = ctypes.WinDLL("advapi32", use_last_error=True)
    get_control = advapi.GetSecurityDescriptorControl
    get_control.argtypes = [ctypes.c_void_p, ctypes.POINTER(wintypes.WORD), ctypes.POINTER(wintypes.DWORD)]
    get_control.restype = wintypes.BOOL
    control, revision = wintypes.WORD(), wintypes.DWORD()
    if not get_control(descriptor, ctypes.byref(control), ctypes.byref(revision)):
        raise ctypes.WinError(ctypes.get_last_error())
    get_dacl = advapi.GetSecurityDescriptorDacl
    get_dacl.argtypes = [ctypes.c_void_p, ctypes.POINTER(wintypes.BOOL), ctypes.POINTER(ctypes.c_void_p), ctypes.POINTER(wintypes.BOOL)]
    get_dacl.restype = wintypes.BOOL
    present, defaulted, acl = wintypes.BOOL(), wintypes.BOOL(), ctypes.c_void_p()
    if not get_dacl(descriptor, ctypes.byref(present), ctypes.byref(acl), ctypes.byref(defaulted)):
        raise ctypes.WinError(ctypes.get_last_error())
    raw = b"absent" if not present.value else b"null"
    if acl:
        size = ctypes.c_uint16.from_address(acl.value + 2).value
        raw = ctypes.string_at(acl, size)
    digest = hashlib.sha256(bytes([bool(control.value & 0x1000)]) + raw).hexdigest()
    return "windows-dacl-v1:" + digest


_NO_SECURITY_CHECK = object()


def ensure_private_file_descriptor(fd):
    """Protect one already-open regular file without resolving its name again."""
    import msvcrt
    from ctypes import wintypes
    kernel, advapi = _api(), ctypes.WinDLL("advapi32", use_last_error=True)
    handle = msvcrt.get_osfhandle(fd)
    info = _info(handle)
    if info.attributes & 0x10 or info.links != 1:
        raise ValueError("Private Windows state must be a regular file with one link")
    signatures = {
        "OpenProcessToken": ([wintypes.HANDLE, wintypes.DWORD, ctypes.POINTER(wintypes.HANDLE)], wintypes.BOOL),
        "GetTokenInformation": ([wintypes.HANDLE, ctypes.c_int, ctypes.c_void_p, wintypes.DWORD, ctypes.POINTER(wintypes.DWORD)], wintypes.BOOL),
        "GetSecurityInfo": ([wintypes.HANDLE, ctypes.c_int, wintypes.DWORD, ctypes.POINTER(ctypes.c_void_p), ctypes.c_void_p,
                            ctypes.c_void_p, ctypes.c_void_p, ctypes.POINTER(ctypes.c_void_p)], wintypes.DWORD),
        "EqualSid": ([ctypes.c_void_p, ctypes.c_void_p], wintypes.BOOL),
        "ConvertSidToStringSidW": ([ctypes.c_void_p, ctypes.POINTER(wintypes.LPWSTR)], wintypes.BOOL),
        "ConvertStringSecurityDescriptorToSecurityDescriptorW": ([wintypes.LPCWSTR, wintypes.DWORD, ctypes.POINTER(ctypes.c_void_p), ctypes.c_void_p], wintypes.BOOL),
        "GetSecurityDescriptorDacl": ([ctypes.c_void_p, ctypes.POINTER(wintypes.BOOL), ctypes.POINTER(ctypes.c_void_p), ctypes.POINTER(wintypes.BOOL)], wintypes.BOOL),
        "SetSecurityInfo": ([wintypes.HANDLE, ctypes.c_int, wintypes.DWORD, ctypes.c_void_p, ctypes.c_void_p, ctypes.c_void_p, ctypes.c_void_p], wintypes.DWORD),
    }
    for name, (arguments, result) in signatures.items():
        getattr(advapi, name).argtypes = arguments
        getattr(advapi, name).restype = result
    kernel.GetCurrentProcess.restype = wintypes.HANDLE
    kernel.ReOpenFile.argtypes = [wintypes.HANDLE, wintypes.DWORD, wintypes.DWORD, wintypes.DWORD]
    kernel.ReOpenFile.restype = wintypes.HANDLE
    kernel.LocalFree.argtypes = [ctypes.c_void_p]
    kernel.LocalFree.restype = ctypes.c_void_p
    token, sid_text = wintypes.HANDLE(), wintypes.LPWSTR()
    owner_descriptor, private_descriptor = ctypes.c_void_p(), ctypes.c_void_p()
    private_handle = None
    if not advapi.OpenProcessToken(kernel.GetCurrentProcess(), 8, ctypes.byref(token)):
        raise ctypes.WinError(ctypes.get_last_error())
    try:
        def token_sid(kind):
            needed = wintypes.DWORD()
            advapi.GetTokenInformation(token, kind, None, 0, ctypes.byref(needed))
            data = ctypes.create_string_buffer(needed.value)
            if not advapi.GetTokenInformation(token, kind, data, needed.value, ctypes.byref(needed)):
                raise ctypes.WinError(ctypes.get_last_error())
            return data, ctypes.cast(data, ctypes.POINTER(ctypes.c_void_p))[0]

        user_data, user = token_sid(1)
        owner = ctypes.c_void_p()
        error = advapi.GetSecurityInfo(handle, 1, 1, ctypes.byref(owner), None, None, None, ctypes.byref(owner_descriptor))
        if error:
            raise ctypes.WinError(error)
        change_owner = not advapi.EqualSid(owner, user)
        if change_owner:
            default_data, default_owner = token_sid(4)
            if not advapi.EqualSid(owner, default_owner):
                raise PermissionError("Private Windows state file is owned by another account")
        access = 0x20000 | 0x40000 | (0x80000 if change_owner else 0)  # READ_CONTROL | WRITE_DAC | WRITE_OWNER
        private_handle = kernel.ReOpenFile(handle, access, 3, 0)
        if private_handle == ctypes.c_void_p(-1).value:
            private_handle = None
            raise ctypes.WinError(ctypes.get_last_error())
        if change_owner:
            error = advapi.SetSecurityInfo(private_handle, 1, 1, user, None, None, None)
            if error:
                raise ctypes.WinError(error)
        if not advapi.ConvertSidToStringSidW(user, ctypes.byref(sid_text)):
            raise ctypes.WinError(ctypes.get_last_error())
        if not advapi.ConvertStringSecurityDescriptorToSecurityDescriptorW(
                _private_state_sddl(sid_text.value), 1, ctypes.byref(private_descriptor), None):
            raise ctypes.WinError(ctypes.get_last_error())
        present, defaulted, acl = wintypes.BOOL(), wintypes.BOOL(), ctypes.c_void_p()
        if not advapi.GetSecurityDescriptorDacl(private_descriptor, ctypes.byref(present), ctypes.byref(acl), ctypes.byref(defaulted)):
            raise ctypes.WinError(ctypes.get_last_error())
        error = advapi.SetSecurityInfo(private_handle, 1, 4 | 0x80000000, None, None, acl, None)
        if error:
            raise ctypes.WinError(error)
    finally:
        if private_handle is not None:
            kernel.CloseHandle(private_handle)
        for descriptor in (owner_descriptor, private_descriptor):
            if descriptor:
                kernel.LocalFree(descriptor)
        if sid_text:
            kernel.LocalFree(ctypes.cast(sid_text, ctypes.c_void_p))
        kernel.CloseHandle(token)


def ensure_private_directory(path):
    """Refuse foreign owners and enforce an actual owner/SYSTEM Windows DACL."""
    from ctypes import wintypes
    kernel = _api()
    advapi = ctypes.WinDLL("advapi32", use_last_error=True)
    signatures = {
        "OpenProcessToken": ([wintypes.HANDLE, wintypes.DWORD, ctypes.POINTER(wintypes.HANDLE)], wintypes.BOOL),
        "GetTokenInformation": ([wintypes.HANDLE, ctypes.c_int, ctypes.c_void_p, wintypes.DWORD, ctypes.POINTER(wintypes.DWORD)], wintypes.BOOL),
        "ConvertSidToStringSidW": ([ctypes.c_void_p, ctypes.POINTER(wintypes.LPWSTR)], wintypes.BOOL),
        "GetSecurityDescriptorOwner": ([ctypes.c_void_p, ctypes.POINTER(ctypes.c_void_p), ctypes.POINTER(wintypes.BOOL)], wintypes.BOOL),
        "EqualSid": ([ctypes.c_void_p, ctypes.c_void_p], wintypes.BOOL),
        "ConvertStringSecurityDescriptorToSecurityDescriptorW": ([wintypes.LPCWSTR, wintypes.DWORD, ctypes.POINTER(ctypes.c_void_p), ctypes.c_void_p], wintypes.BOOL),
        "SetSecurityInfo": ([wintypes.HANDLE, ctypes.c_int, wintypes.DWORD, ctypes.c_void_p, ctypes.c_void_p, ctypes.c_void_p, ctypes.c_void_p], wintypes.DWORD),
    }
    for name, (arguments, result) in signatures.items():
        getattr(advapi, name).argtypes = arguments
        getattr(advapi, name).restype = result
    kernel.GetCurrentProcess.restype = wintypes.HANDLE
    kernel.LocalFree.argtypes = [ctypes.c_void_p]
    kernel.LocalFree.restype = ctypes.c_void_p
    token = wintypes.HANDLE()
    if not advapi.OpenProcessToken(kernel.GetCurrentProcess(), 8, ctypes.byref(token)):
        raise ctypes.WinError(ctypes.get_last_error())
    sid_text = wintypes.LPWSTR()
    descriptor = ctypes.c_void_p()
    try:
        needed = wintypes.DWORD()
        advapi.GetTokenInformation(token, 1, None, 0, ctypes.byref(needed))
        data = ctypes.create_string_buffer(needed.value)
        if not advapi.GetTokenInformation(token, 1, data, needed.value, ctypes.byref(needed)):
            raise ctypes.WinError(ctypes.get_last_error())
        sid = ctypes.cast(data, ctypes.POINTER(ctypes.c_void_p))[0]
        if not advapi.ConvertSidToStringSidW(sid, ctypes.byref(sid_text)):
            raise ctypes.WinError(ctypes.get_last_error())
        with pinned_directory(path, create=True) as pinned:
            existing = _security_descriptor(pinned, 1)
            owner = ctypes.c_void_p()
            defaulted = wintypes.BOOL()
            if not advapi.GetSecurityDescriptorOwner(existing, ctypes.byref(owner), ctypes.byref(defaulted)):
                raise ctypes.WinError(ctypes.get_last_error())
            if not advapi.EqualSid(sid, owner):
                # Administrative tokens can legally create objects owned by
                # their exact TokenOwner group. Bind that default owner to this
                # user with WRITE_OWNER on a fixed handle; no other account or
                # arbitrary administrator-group owner is accepted.
                default_needed = wintypes.DWORD()
                advapi.GetTokenInformation(token, 4, None, 0, ctypes.byref(default_needed))
                default_data = ctypes.create_string_buffer(default_needed.value)
                if not advapi.GetTokenInformation(token, 4, default_data, default_needed.value, ctypes.byref(default_needed)):
                    raise ctypes.WinError(ctypes.get_last_error())
                default_owner = ctypes.cast(default_data, ctypes.POINTER(ctypes.c_void_p))[0]
                if not advapi.EqualSid(default_owner, owner):
                    raise PermissionError("Private Windows state directory is owned by another account")
                private_handle = _open(pinned, directory=True, access=0x80000 | 0x20000)
                try:
                    error = advapi.SetSecurityInfo(private_handle, 1, 1, sid, None, None, None)
                    if error:
                        raise ctypes.WinError(error)
                finally:
                    kernel.CloseHandle(private_handle)
            sddl = _private_state_sddl(sid_text.value, directory=True)
            if not advapi.ConvertStringSecurityDescriptorToSecurityDescriptorW(sddl, 1, ctypes.byref(descriptor), None):
                raise ctypes.WinError(ctypes.get_last_error())
            _set_dacl(pinned, descriptor, protected=True)
        return Path(path)
    finally:
        if descriptor:
            kernel.LocalFree(descriptor)
        if sid_text:
            kernel.LocalFree(ctypes.cast(sid_text, ctypes.c_void_p))
        kernel.CloseHandle(token)


class _FileInfo(ctypes.Structure):
    # DWORD and FILETIME fields remain 32 bit on Windows x64.
    _fields_ = [(name, ctypes.c_uint32) for name in (
        "attributes", "creation_low", "creation_high", "access_low", "access_high",
        "write_low", "write_high", "volume", "size_high", "size_low", "links", "index_high", "index_low")]


def _info(handle):
    kernel = _api()
    kernel.GetFileInformationByHandle.argtypes = [ctypes.c_void_p, ctypes.POINTER(_FileInfo)]
    kernel.GetFileInformationByHandle.restype = ctypes.c_int
    info = _FileInfo()
    if not kernel.GetFileInformationByHandle(handle, ctypes.byref(info)):
        raise ctypes.WinError(ctypes.get_last_error())
    if info.attributes & 0x400:
        raise ValueError("Windows reparse points are not permitted in protected workspaces")
    return info


def _open(path, *, directory=False, read=False, create=False, access=None):
    kernel = _api()
    # A zero-access metadata handle does not participate in Windows sharing
    # checks. Request actual read/traverse access so omitting SHARE_DELETE
    # really prevents an ancestor from being renamed while it is pinned.
    desired = access if access is not None else (0x80000000 if read else (0x40000000 if create else (0x20 | 0x80 if directory else 0x80)))
    handle = kernel.CreateFileW(_api_path(path), desired,
                               1 if read else (0 if create else 3), None, 1 if create else 3,
                               0x00200000 | (0x02000000 if directory else 0), None)
    if handle == ctypes.c_void_p(-1).value:
        raise ctypes.WinError(ctypes.get_last_error())
    try:
        info = _info(handle)
        if bool(info.attributes & 0x10) != directory:
            raise ValueError("Windows path is not the required regular file/directory")
        return handle
    except BaseException:
        kernel.CloseHandle(handle)
        raise


def _absolute_components(path):
    from makewand.windows_paths import ordinary_local_abspath
    absolute = ordinary_local_abspath(path)
    drive, tail = ntpath.splitdrive(absolute)
    # Local drive roots only: network shares and device namespaces cannot supply
    # the local locking and identity guarantees required by this backend.
    if not re.fullmatch(r"[A-Za-z]:", drive) or not tail.startswith("\\"):
        raise ValueError("Safe Windows operations require a local drive path")
    parts = [part for part in tail.split("\\") if part]
    for part in parts:
        relative_parts(part)
    return drive + "\\", parts


def _api_path(path):
    """Validate the original namespace, then extend only the Win32 call path."""
    from makewand.windows_paths import extended_local_path
    root, parts = _absolute_components(path)
    return extended_local_path(ntpath.join(root, *parts))


@contextlib.contextmanager
def pinned_directory(path, *, create=False):
    kernel = _api()
    root, parts = _absolute_components(path)
    handles = []
    current = root
    try:
        handles.append(_open(current, directory=True))
        for part in parts:
            current = ntpath.join(current, part)
            if create:
                try:
                    os.mkdir(_api_path(current))
                except FileExistsError:
                    pass
            handles.append(_open(current, directory=True))
        yield Path(current)
    finally:
        for handle in reversed(handles):
            kernel.CloseHandle(handle)


def _set_file_information(handle, kind, buffer, size):
    from ctypes import wintypes
    setter = _api().SetFileInformationByHandle
    setter.argtypes = [wintypes.HANDLE, ctypes.c_int, ctypes.c_void_p, wintypes.DWORD]
    setter.restype = wintypes.BOOL
    if not setter(handle, kind, buffer, size):
        raise ctypes.WinError(ctypes.get_last_error())


def _set_nt_file_information(handle, kind, buffer, size):
    """Set handle-relative file information through the user-mode NT service."""
    from ctypes import wintypes

    class IoStatusBlock(ctypes.Structure):
        _fields_ = [("status_or_pointer", ctypes.c_void_p), ("information", ctypes.c_size_t)]

    ntdll = ctypes.WinDLL("ntdll", use_last_error=True)
    setter = ntdll.NtSetInformationFile
    setter.argtypes = [wintypes.HANDLE, ctypes.POINTER(IoStatusBlock), ctypes.c_void_p,
                       wintypes.ULONG, ctypes.c_int]
    setter.restype = ctypes.c_int32  # NTSTATUS is signed 32 bit on x64 too.
    status = setter(handle, ctypes.byref(IoStatusBlock()), buffer, size, kind)
    _check_nt_status(status)


def _check_nt_status(status):
    from ctypes import wintypes
    if status != 0:
        # The handles are synchronous; never publish a commit for an incomplete
        # STATUS_PENDING result or an informational status we did not expect.
        ntdll = ctypes.WinDLL("ntdll", use_last_error=True)
        converter = ntdll.RtlNtStatusToDosError
        converter.argtypes = [ctypes.c_int32]
        converter.restype = wintypes.ULONG
        raise ctypes.WinError(converter(status))
