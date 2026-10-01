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


def _security_descriptor(path, information):
    from ctypes import wintypes
    advapi = ctypes.WinDLL("advapi32", use_last_error=True)
    get = advapi.GetFileSecurityW
    get.argtypes = [wintypes.LPCWSTR, wintypes.DWORD, ctypes.c_void_p, wintypes.DWORD, ctypes.POINTER(wintypes.DWORD)]
    get.restype = wintypes.BOOL
    needed = wintypes.DWORD()
    get(str(path), information, None, 0, ctypes.byref(needed))
    if not needed.value:
        raise ctypes.WinError(ctypes.get_last_error())
    descriptor = ctypes.create_string_buffer(needed.value)
    if not get(str(path), information, descriptor, needed.value, ctypes.byref(needed)):
        raise ctypes.WinError(ctypes.get_last_error())
    return descriptor


def _set_dacl(path, descriptor, *, protected=None):
    from ctypes import wintypes
    advapi = ctypes.WinDLL("advapi32", use_last_error=True)
    setter = advapi.SetFileSecurityW
    setter.argtypes = [wintypes.LPCWSTR, wintypes.DWORD, ctypes.c_void_p]
    setter.restype = wintypes.BOOL
    if protected is None:
        get_control = advapi.GetSecurityDescriptorControl
        get_control.argtypes = [ctypes.c_void_p, ctypes.POINTER(wintypes.WORD), ctypes.POINTER(wintypes.DWORD)]
        get_control.restype = wintypes.BOOL
        control, revision = wintypes.WORD(), wintypes.DWORD()
        if not get_control(descriptor, ctypes.byref(control), ctypes.byref(revision)):
            raise ctypes.WinError(ctypes.get_last_error())
        protected = bool(control.value & 0x1000)
    if not setter(str(path), 4 | (0x80000000 if protected else 0x20000000), descriptor):
        raise ctypes.WinError(ctypes.get_last_error())


def dacl_fingerprint(path):
    """Stable ACL bytes for preservation checks; no claim of Unix permissions."""
    from ctypes import wintypes
    descriptor = _security_descriptor(path, 4)
    advapi = ctypes.WinDLL("advapi32", use_last_error=True)
    getter = advapi.GetSecurityDescriptorDacl
    getter.argtypes = [ctypes.c_void_p, ctypes.POINTER(wintypes.BOOL), ctypes.POINTER(ctypes.c_void_p), ctypes.POINTER(wintypes.BOOL)]
    getter.restype = wintypes.BOOL
    present, defaulted, acl = wintypes.BOOL(), wintypes.BOOL(), ctypes.c_void_p()
    if not getter(descriptor, ctypes.byref(present), ctypes.byref(acl), ctypes.byref(defaulted)):
        raise ctypes.WinError(ctypes.get_last_error())
    if not present.value or not acl:
        return "absent" if not present.value else "null"
    size = ctypes.c_uint16.from_address(acl.value + 2).value
    return hashlib.sha256(ctypes.string_at(acl, size)).hexdigest()


def _open_file_security_descriptor(handle):
    """Copy the DACL descriptor of one captured object, never a fresh pathname."""
    from ctypes import wintypes
    advapi = ctypes.WinDLL("advapi32", use_last_error=True)
    getter = advapi.GetSecurityInfo
    getter.argtypes = [wintypes.HANDLE, ctypes.c_int, wintypes.DWORD,
                      ctypes.c_void_p, ctypes.c_void_p, ctypes.c_void_p, ctypes.c_void_p,
                      ctypes.POINTER(ctypes.c_void_p)]
    getter.restype = wintypes.DWORD
    descriptor = ctypes.c_void_p()
    error = getter(handle, 1, 4, None, None, None, None, ctypes.byref(descriptor))
    if error:
        raise ctypes.WinError(error)
    try:
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
    finally:
        kernel = _api()
        kernel.LocalFree.argtypes = [ctypes.c_void_p]
        kernel.LocalFree.restype = ctypes.c_void_p
        kernel.LocalFree(descriptor)


def _application_security_descriptor(path):
    path = Path(path)
    relative_parts(path.name)
    with pinned_directory(path.parent):
        handle = _open(path, access=0x20000 | 0x80)  # READ_CONTROL | READ_ATTRIBUTES
        try:
            return _open_file_security_descriptor(handle)
        finally:
            _api().CloseHandle(handle)


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


def application_security(path):
    """Host-only transaction seal for the exact DACL and protected flag."""
    return _application_security_value(_application_security_descriptor(path))


def application_security_descriptor(path):
    """Bounded original DACL for rollback in a different private backup tree."""
    descriptor = _application_security_descriptor(path)
    # create_string_buffer adds a terminator; exclude it from the descriptor.
    return base64.b64encode(descriptor.raw[:-1]).decode("ascii")


def _restore_security_descriptor(value):
    if not isinstance(value, str) or len(value) > 87384:
        raise ValueError("Invalid Windows rollback security descriptor")
    try:
        data = base64.b64decode(value, validate=True)
    except (ValueError, TypeError) as error:
        raise ValueError("Invalid Windows rollback security encoding") from error
    if not 20 <= len(data) <= 65536:
        raise ValueError("Invalid Windows rollback security descriptor length")
    # Validate relative offsets before asking the OS to parse private recovery
    # evidence: corrupted offsets must never point outside this owned buffer.
    control = int.from_bytes(data[2:4], "little")
    offsets = [int.from_bytes(data[index:index + 4], "little") for index in (4, 8, 12, 16)]
    if data[0] != 1 or not control & 0x8000 or any(offsets[:3]):
        raise ValueError("Rollback security must be a relative DACL-only descriptor")
    dacl = offsets[3]
    if dacl:
        if dacl < 20 or dacl > len(data) - 8:
            raise ValueError("Invalid rollback DACL offset")
        size = int.from_bytes(data[dacl + 2:dacl + 4], "little")
        count = int.from_bytes(data[dacl + 4:dacl + 6], "little")
        if size < 8 or dacl + size > len(data):
            raise ValueError("Invalid rollback DACL extent")
        offset = dacl + 8
        for _ in range(count):
            if offset + 4 > dacl + size:
                raise ValueError("Invalid rollback DACL entry")
            entry_size = int.from_bytes(data[offset + 2:offset + 4], "little")
            if entry_size < 4 or offset + entry_size > dacl + size:
                raise ValueError("Invalid rollback DACL entry extent")
            offset += entry_size
    descriptor = ctypes.create_string_buffer(data)
    from ctypes import wintypes
    validator = ctypes.WinDLL("advapi32", use_last_error=True).IsValidSecurityDescriptor
    validator.argtypes = [ctypes.c_void_p]
    validator.restype = wintypes.BOOL
    if not validator(descriptor):
        raise ValueError("Invalid Windows rollback security descriptor")
    return descriptor


def validate_application_security_descriptor(value, expected_security):
    """Validate all rollback evidence before any entry may be restored."""
    descriptor = _restore_security_descriptor(value)
    if (not isinstance(expected_security, str)
            or _application_security_value(descriptor) != expected_security):
        raise ValueError("Windows rollback security descriptor does not match its frozen DACL")


_NO_SECURITY_CHECK = object()


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
            sddl = "D:P(A;OICI;FA;;;" + sid_text.value + ")(A;OICI;FA;;;SY)"
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
    desired = access if access is not None else (0x80000000 if read else (0x40000000 if create else 0))
    handle = kernel.CreateFileW(str(path), desired,
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
    absolute = os.path.abspath(os.fspath(path))
    drive, tail = ntpath.splitdrive(absolute)
    # Local drive roots only: network shares and device namespaces cannot supply
    # the local locking and identity guarantees required by this backend.
    if not re.fullmatch(r"[A-Za-z]:", drive) or not tail.startswith("\\"):
        raise ValueError("Safe Windows operations require a local drive path")
    parts = [part for part in tail.split("\\") if part]
    for part in parts:
        relative_parts(part)
    return drive + "\\", parts


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
                    os.mkdir(current)
                except FileExistsError:
                    pass
            handles.append(_open(current, directory=True))
        yield Path(current)
    finally:
        for handle in reversed(handles):
            kernel.CloseHandle(handle)


@contextlib.contextmanager
def regular_reader(path):
    """Read one stable file identity; deny concurrent data writes and rename."""
    import msvcrt
    path = Path(path)
    relative_parts(path.name)
    with pinned_directory(path.parent):
        handle = _open(path, read=True)
        try:
            fd = msvcrt.open_osfhandle(handle, os.O_RDONLY | os.O_BINARY)
        except BaseException:
            _api().CloseHandle(handle)
            raise
        with os.fdopen(fd, "rb", buffering=0) as stream:
            yield stream


def inspect_file(path, *, deadline=None):
    with regular_reader(path) as stream:
        before = os.fstat(stream.fileno())
        digest = hashlib.sha256()
        while True:
            if deadline:
                deadline()
            chunk = stream.read(65536)
            if not chunk:
                break
            digest.update(chunk)
        info = os.fstat(stream.fileno())
        def signature(entry):
            return (entry.st_dev, entry.st_ino, entry.st_size,
                    entry.st_mode, entry.st_mtime_ns, entry.st_ctime_ns)
        if signature(before) != signature(info):
            raise ValueError("Windows file metadata changed while hashing")
        return {"sha256": digest.hexdigest(), "mode": stat.S_IMODE(info.st_mode)}


def manifest(workspace, *, input_snapshot=False):
    """Scan through pinned directories and refuse every reparse entry."""
    import time
    root = Path(os.path.abspath(workspace))
    result = {}
    started = time.monotonic()
    total_bytes = 0

    def scan(directory):
        nonlocal total_bytes
        with pinned_directory(directory):
            for child in os.scandir(directory):
                if directory == root and child.name == ".git":
                    continue
                relative = Path(child.path).relative_to(root).as_posix()
                relative_parts(relative)
                info = child.stat(follow_symlinks=False)
                if getattr(info, "st_file_attributes", 0) & 0x400:
                    raise ValueError("Windows workspace contains a reparse point: " + relative)
                if stat.S_ISDIR(info.st_mode):
                    scan(Path(child.path))
                elif stat.S_ISREG(info.st_mode):
                    total_bytes += info.st_size
                    if total_bytes > 512 * 1024 * 1024 or len(result) >= 100000 or time.monotonic() - started > 10:
                        raise OSError("Windows manifest exceeded its input budget")
                    record = inspect_file(child.path)
                    result[relative] = ("file", record["sha256"], record["mode"]) if input_snapshot else record
                else:
                    raise ValueError("Windows workspace contains a nonregular input: " + relative)
    scan(root)
    return result


def validate_target(workspace, relative):
    parts = relative_parts(relative)
    root = Path(os.path.abspath(workspace))
    # Pin the existing prefix, including a leaf directory or regular file.
    with pinned_directory(root):
        current = root
        for part in parts:
            current /= part
            if not os.path.lexists(current):
                break
            handle = _open(current, directory=current.is_dir())
            _api().CloseHandle(handle)
    return root.joinpath(*parts)


def atomic_copy(workspace, relative, source, expected=None, *, restore_acl=False, temporary_name=None,
                before_replace=None, restore_security=None, _copy_source_acl=True):
    parts = relative_parts(relative)
    destination = Path(os.path.abspath(workspace)).joinpath(*parts)
    temp_name = temporary_name if temporary_name is not None else ".makewand-" + uuid.uuid4().hex
    if (not isinstance(temp_name, str) or len(temp_name) != len(".makewand-") + 32
            or not temp_name.startswith(".makewand-")
            or any(character not in "0123456789abcdef" for character in temp_name[len(".makewand-"):])):
        raise ValueError("invalid registered application temporary name")
    temporary = destination.parent / temp_name
    created = False
    with pinned_directory(destination.parent, create=True), regular_reader(source) as src:
        info = os.fstat(src.fileno())
        mode = stat.S_IMODE(info.st_mode)
        try:
            import msvcrt
            handle = _open(temporary, create=True)
            try:
                fd = msvcrt.open_osfhandle(handle, os.O_WRONLY | os.O_BINARY)
            except BaseException:
                _api().CloseHandle(handle)
                raise
            created = True
            digest = hashlib.sha256()
            with os.fdopen(fd, "wb", buffering=0) as dst:
                while chunk := src.read(65536):
                    digest.update(chunk)
                    dst.write(chunk)
                if expected is not None and expected != {"sha256": digest.hexdigest(), "mode": mode}:
                    raise ValueError("candidate changed during Windows apply")
                os.fsync(dst.fileno())
            # chmod on Windows preserves its supported readonly attribute. ACLs
            # come from the target directory, never from the model's file.
            os.chmod(temporary, mode)
            if restore_security is not None:
                _set_dacl(temporary, _restore_security_descriptor(restore_security))
            elif _copy_source_acl and (expected is None or restore_acl):
                # Legacy host copies retain source ACLs. Transaction rollback
                # supplies its separately sealed original security descriptor.
                _set_dacl(temporary, _security_descriptor(source, 4))
            elif os.path.lexists(destination):
                handle = _open(destination)
                try:
                    # Preserve the original file DACL. A replacement must not
                    # widen a restrictive ACL by inheriting its parent's ACL.
                    _set_dacl(temporary, _security_descriptor(destination, 4))
                finally:
                    _api().CloseHandle(handle)
            if before_replace is not None:
                before_replace(application_security(temporary))
            replace_file(temporary, destination)
            created = False
        finally:
            if created:
                os.chmod(temporary, stat.S_IWRITE)
                os.unlink(temporary)


def atomic_remove(workspace, relative, *, directory=False, expected_security=_NO_SECURITY_CHECK):
    parts = relative_parts(relative)
    target = Path(os.path.abspath(workspace)).joinpath(*parts)
    with pinned_directory(target.parent):
        try:
            handle = _open(target, directory=directory, access=0x10000 | 0x100 | 0x80 | (0x20000 if expected_security is not _NO_SECURITY_CHECK else 0))
        except FileNotFoundError:
            return
        try:
            if expected_security is not _NO_SECURITY_CHECK:
                security = _application_security_value(_open_file_security_descriptor(handle))
                if expected_security is None or security != expected_security:
                    raise ValueError("Windows application security changed before removal")
            flags = ctypes.c_uint32(1 | 2 | 0x10)  # DELETE | POSIX_SEMANTICS | IGNORE_READONLY_ATTRIBUTE
            _set_file_information(handle, 21, ctypes.byref(flags), ctypes.sizeof(flags))
        finally:
            _api().CloseHandle(handle)


def copy_backup(source, destination):
    destination = Path(destination)
    # Private preimages inherit only the private state DACL. Their original
    # workspace ACL is retained separately in the durable host-only journal.
    atomic_copy(destination.parent, destination.name, Path(source), _copy_source_acl=False)


def replace_file(temporary, destination):
    """Handle-relative atomic replacement, including readonly target files.

    FileRenameInfoEx ignores the readonly attribute without temporarily chmoding
    a user file. A process exit can therefore leave only its preimage/postimage,
    never a permission-only intermediate state. Requires Windows 10 or newer.
    """
    temporary, destination = Path(temporary), Path(destination)
    relative_parts(temporary.name)
    relative_parts(destination.name)
    kernel = _api()
    with pinned_directory(temporary.parent), pinned_directory(destination.parent):
        # The source is exclusively created by the caller; validation ensures
        # reparse points cannot enter either endpoint before publication.
        if os.path.lexists(destination):
            handle = _open(destination)
            kernel.CloseHandle(handle)
        source = _open(temporary, access=0x10000 | 0x80)
        parent = None
        try:
            parent = _open(destination.parent, directory=True, access=0x20 | 0x80)
            class RenameInfo(ctypes.Structure):
                _fields_ = [("flags", ctypes.c_uint32), ("root", ctypes.c_void_p),
                            ("name_length", ctypes.c_uint32), ("name", ctypes.c_uint16 * 1)]
            encoded = destination.name.encode("utf-16-le")
            buffer = ctypes.create_string_buffer(ctypes.sizeof(RenameInfo) + len(encoded))
            info = RenameInfo.from_buffer(buffer)
            info.flags = 1 | 0x40  # REPLACE_IF_EXISTS | IGNORE_READONLY_ATTRIBUTE
            info.root = parent
            info.name_length = len(encoded)
            ctypes.memmove(ctypes.addressof(buffer) + RenameInfo.name.offset, encoded, len(encoded))
            _set_file_information(source, 22, buffer, len(buffer))
        finally:
            if parent is not None:
                kernel.CloseHandle(parent)
            kernel.CloseHandle(source)


def _set_file_information(handle, kind, buffer, size):
    from ctypes import wintypes
    setter = _api().SetFileInformationByHandle
    setter.argtypes = [wintypes.HANDLE, ctypes.c_int, ctypes.c_void_p, wintypes.DWORD]
    setter.restype = wintypes.BOOL
    if not setter(handle, kind, buffer, size):
        raise ctypes.WinError(ctypes.get_last_error())
