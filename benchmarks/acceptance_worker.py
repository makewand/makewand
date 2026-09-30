"""Execute candidate code in a child; never execute parent assertions."""
import contextlib
import importlib.util
import json
import sys
import os
import signal
from pathlib import Path


def _seal_attributes(target):
    """Bind a definition snapshot without relying on candidate global bindings."""
    from types import FunctionType
    raw_dict, raw_type, raw_vars, raw_set, raw_all = dict, type, vars, set, all
    members = raw_dict(raw_vars(target))
    functions = [(value, value.__code__, value.__defaults__, value.__kwdefaults__)
                 for value in members.values() if raw_type(value) is FunctionType]

    def unchanged():
        current = raw_vars(target)
        return (raw_set(current) == raw_set(members)
                and raw_all(current[name] is original for name, original in members.items())
                and raw_all(function.__code__ is code and function.__defaults__ is defaults
                            and function.__kwdefaults__ is kwdefaults
                            for function, code, defaults, kwdefaults in functions))
    return unchanged


def _prepare_key_probe(case):
    """Construct fixed types and bind their observer before candidate imports."""
    from copy import deepcopy

    if case not in ("identity_recursive", "identity_replace", "equal_recursive"):
        raise ValueError("unknown deep-config v4 case")

    class LockedProbeType(type):
        def __setattr__(cls, name, value):
            raise TypeError("trusted probe definitions cannot be changed")

        def __delattr__(cls, name):
            raise TypeError("trusted probe definitions cannot be changed")

    class IdentityKey(metaclass=LockedProbeType):
        __slots__ = ("label", "notes")
        __hash__ = object.__hash__
        __eq__ = object.__eq__

        def __init__(self, label):
            self.label = label
            self.notes = ["initial-key"]

        def __deepcopy__(self, memo, _type=type, _copy=deepcopy, _id=id):
            copied = _type(self)(self.label)
            memo[_id(self)] = copied
            copied.notes = _copy(self.notes, memo)
            return copied

    class EqualityKey(IdentityKey):
        __slots__ = ()

        def __hash__(self, _hash=hash):
            return _hash(self.label)

        def __eq__(self, other, _type=type):
            return _type(self) is _type(other) and self.label == other.label

    # type.__setattr__ can bypass the metaclass; verify method/code identities
    # too. The observer uses captured builtins, never candidate-selected hooks.
    raw_type, raw_dict, raw_list, raw_str = type, dict, list, str
    raw_getattr, raw_all, raw_any = getattr, all, any
    raw_len, raw_next, raw_iter = len, next, iter
    seals = [_seal_attributes(cls) for cls in (LockedProbeType, IdentityKey, EqualityKey)]
    key_type = EqualityKey if case == "equal_recursive" else IdentityKey
    base_key = key_type("same-key")
    override_key = key_type("same-key") if case == "equal_recursive" else base_key
    base_value = {"discarded": ["base"]} if case == "identity_replace" else {"nested": {"base": ["base"]}}
    override_value = ["override"] if case == "identity_replace" else {"nested": {"override": ["override"]}}
    payload = {"base": {base_key: base_value}, "override": {override_key: override_value}}
    original_keys = [base_key] if base_key is override_key else [base_key, override_key]

    def primitive(value, depth=0):
        if depth > 20:
            raise ValueError("probe value exceeds depth bound")
        if raw_type(value) is raw_dict:
            if raw_any(raw_type(key) is not raw_str for key in raw_dict.keys(value)):
                raise ValueError("unexpected probe value key")
            return {key: primitive(child, depth + 1) for key, child in raw_dict.items(value)}
        if raw_type(value) is raw_list:
            return [primitive(child, depth + 1) for child in value]
        if raw_type(value) is raw_str:
            return value
        raise ValueError("unexpected probe value type")

    def input_snapshot():
        if raw_any(raw_type(key) is not key_type or raw_type(key.notes) is not raw_list
                   or raw_type(key.label) is not raw_str or key.label != "same-key" for key in original_keys):
            raise ValueError("probe input key type changed")
        if (raw_type(payload["base"]) is not raw_dict or raw_type(payload["override"]) is not raw_dict
                or raw_len(payload["base"]) != 1 or raw_len(payload["override"]) != 1
                or raw_next(raw_iter(raw_dict.keys(payload["base"]))) is not base_key
                or raw_next(raw_iter(raw_dict.keys(payload["override"]))) is not override_key
                or raw_dict.__getitem__(payload["base"], base_key) is not base_value
                or raw_dict.__getitem__(payload["override"], override_key) is not override_value):
            raise ValueError("probe input keys changed")
        return [primitive(key.notes) for key in original_keys], primitive(base_value), primitive(override_value)

    original = input_snapshot()

    def mutate_lists(value, marker):
        if raw_type(value) is raw_dict:
            for child in raw_dict.values(value):
                mutate_lists(child, marker)
        elif raw_type(value) is raw_list:
            for child in raw_list(value):
                mutate_lists(child, marker)
            raw_list.append(value, marker)

    def observe(module, function_name):
        result = raw_getattr(module, function_name)(payload)
        if not raw_all(unchanged() for unchanged in seals):
            raise ValueError("trusted probe type was modified")
        after_call = input_snapshot()
        if raw_type(result) is not raw_dict:
            raise ValueError("probe result must be a plain dictionary")
        entries = raw_list(raw_dict.items(result))
        if raw_len(entries) != 1:
            raise ValueError("same original key did not merge into one entry")
        output_key, output_value = entries[0]
        if (raw_type(output_key) is not key_type or raw_type(output_key.notes) is not raw_list
                or raw_type(output_key.label) is not raw_str):
            raise ValueError("probe result key type changed")
        before_result = primitive(output_value)
        before_key = {"label": output_key.label, "notes": primitive(output_key.notes)}
        shared_key = raw_any(output_key is key for key in original_keys)
        raw_list.append(output_key.notes, "output-key-mutation")
        mutate_lists(output_value, "output-value-mutation")
        output_to_input = input_snapshot() == after_call
        output_snapshot = primitive(output_key.notes), primitive(output_value)
        for key in original_keys:
            raw_list.append(key.notes, "input-key-mutation")
        mutate_lists(base_value, "input-value-mutation")
        mutate_lists(override_value, "input-value-mutation")
        input_to_output = output_snapshot == (primitive(output_key.notes), primitive(output_value))
        return {"schema": 4, "kind": "deep_config_key_observation", "case": case,
                "entries": raw_len(entries), "result": before_result, "key_state": before_key,
                "key_type_preserved": True,
                "shared_input_key": shared_key, "input_unchanged": after_call == original,
                "output_to_input_detached": output_to_input, "input_to_output_detached": input_to_output,
                "probe_types_unchanged": raw_all(unchanged() for unchanged in seals)}

    return observe


def main():
    sys.dont_write_bytecode = True
    # A worker must not outlive its trusted driver after the process group dies.
    parent_pid = int(os.environ.get("MAKEWAND_ACCEPTANCE_PARENT_PID", "0"))
    if sys.platform == "linux" and parent_pid:
        import ctypes
        libc = ctypes.CDLL(None, use_errno=True)
        if libc.prctl(1, signal.SIGKILL, 0, 0, 0) != 0 or os.getppid() != parent_pid:
            raise SystemExit("acceptance parent disappeared")
    if os.name == "posix":
        import resource
        resource.setrlimit(resource.RLIMIT_FSIZE, (1024 * 1024, 1024 * 1024))
        resource.setrlimit(resource.RLIMIT_CPU, (5, 6))
        resource.setrlimit(resource.RLIMIT_AS, (256 * 1024 * 1024, 256 * 1024 * 1024))
    request = json.loads(sys.stdin.read())
    operation = request.get("operation")
    module_path, function_name = Path(sys.argv[1]), sys.argv[2]
    # Bind observer and output methods before untrusted imports. A candidate
    # return value can never masquerade as this observer's response dictionary.
    observer = None
    if operation == "deep_config_keys_v4":
        if (set(request) != {"operation", "case", "nonce"}
                or type(request["nonce"]) is not str or len(request["nonce"]) != 32
                or any(character not in "0123456789abcdef" for character in request["nonce"])):
            raise SystemExit("invalid deep-config v4 request")
        observer = _prepare_key_probe(request["case"])
    elif operation is not None:
        raise SystemExit("unknown acceptance operation")
    encoder = json.JSONEncoder(ensure_ascii=False, allow_nan=False)
    encode, emit = encoder.encode, sys.stdout.write
    all_unchanged = all
    serializer_guards = [_seal_attributes(target) for target in
                         (json, json.JSONEncoder, sys.modules[json.JSONEncoder.__module__])] if observer else []
    try:
        with contextlib.redirect_stdout(sys.stderr):
            sys.path.insert(0, str(module_path.resolve().parent))
            spec = importlib.util.spec_from_file_location("candidate", module_path)
            module = importlib.util.module_from_spec(spec)
            spec.loader.exec_module(module)
            if observer is not None:
                response = observer(module, function_name)
                if not all_unchanged(unchanged() for unchanged in serializer_guards):
                    raise ValueError("trusted observation serializer was modified")
                response["nonce"] = request["nonce"]
            else:
                values = request["values"]
                argument = iter(values) if request.get("iterator") else values
                try:
                    result = getattr(module, function_name)(argument)
                except ValueError:
                    response = {"schema": 1, "kind": "value_error", "input": values}
                else:
                    if request.get("verify_detached"):
                        original_result = json.loads(json.dumps(result))

                        def clear_mutable(value):
                            if isinstance(value, dict):
                                for child in list(value.values()):
                                    clear_mutable(child)
                                value.clear()
                            elif isinstance(value, list):
                                for child in list(value):
                                    clear_mutable(child)
                                value.clear()
                        clear_mutable(result)
                        result = original_result
                    response = {"schema": 1, "kind": "result", "input": values, "result": result}
    except BaseException as error:
        print(f"candidate execution failed: {type(error).__name__}: {error}", file=sys.stderr)
        raise SystemExit(1)
    emit(encode(response) + "\n")


if __name__ == "__main__":
    main()
