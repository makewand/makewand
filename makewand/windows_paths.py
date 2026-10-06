"""Extended Win32 call paths without changing logical workspace identities."""
import ntpath
import os
import re


def _validate_components(tail, *, navigation=False):
    reserved = re.compile(r"^(?:CON|PRN|AUX|NUL|COM[1-9¹²³]|LPT[1-9¹²³])(?:\..*)?$", re.I)
    for part in tail.replace("/", "\\").split("\\"):
        if navigation and part in (".", ".."):
            continue
        if part and (part[-1:] in (" ", ".") or reserved.fullmatch(part)
                     or any(ord(char) < 32 or char in '<>:"|?*' for char in part)):
            raise ValueError("Windows filesystem path contains an ambiguous component")


def ordinary_local_abspath(path):
    """Validate original names and the normalized ordinary local drive path."""
    value = os.fspath(path)
    if not isinstance(value, str) or "\0" in value:
        raise ValueError("Windows filesystem paths must be ordinary text paths")
    # On Windows, abspath calls GetFullPathNameW, which can erase trailing dots
    # and spaces. Reject the original spelling before any such normalization.
    input_drive, input_tail = ntpath.splitdrive(value)
    if input_drive and not re.fullmatch(r"[A-Za-z]:", input_drive):
        raise ValueError("Windows filesystem operations require an ordinary local drive path")
    _validate_components(input_tail, navigation=True)
    absolute = ntpath.abspath(value)
    drive, tail = ntpath.splitdrive(absolute)
    if not re.fullmatch(r"[A-Za-z]:", drive) or not tail.startswith("\\"):
        raise ValueError("Windows filesystem operations require an ordinary local drive path")
    _validate_components(tail)
    return absolute


def extended_local_path(path):
    """Convert a validated ordinary local path for one filesystem API call.

    Validate before adding the prefix: Win32 no longer normalizes separators or
    names afterwards. UNC and caller-supplied device namespaces remain refused.
    """
    return "\\\\?\\" + ordinary_local_abspath(path)


def windows_git_directory(path):
    """A canonical ordinary startup directory within Git's MAX_PATH bound.

    Git for Windows expands short aliases into fixed MAX_PATH getcwd buffers.
    Long relative file names still work with command-local core.longpaths, but
    an overlong workspace root must fail before any copy or Git mutation.
    """
    ordinary = ordinary_local_abspath(path)
    canonical = ordinary_local_abspath(ntpath.realpath(ordinary))
    if len(canonical.encode("utf-16-le")) // 2 >= 260:
        raise OSError("Git for Windows requires a canonical workspace root shorter than 260 UTF-16 code units")
    return canonical


def filesystem_path(path):
    """An API-only representation; retain the ordinary path in user metadata."""
    return extended_local_path(path) if os.name == "nt" else os.fspath(path)
