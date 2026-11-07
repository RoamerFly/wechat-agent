"""Stage only verified official plugin source files into a portable catalog.

The catalog intentionally contains package descriptors and their indexed source
only. It never packages Python environments, models, account data, generated
outputs, or installed plugin state.
"""
import argparse
import hashlib
import json
import os
from pathlib import Path, PurePosixPath
import re
import shutil
import sys
import tempfile


IDENTIFIER = re.compile(r"^[a-z][a-z0-9-]{0,63}$")
# These are host-side declarative metadata tests/source, deliberately adjacent
# to package directories. They are not plugin package members and must never be
# copied into a portable runtime catalog.
ROOT_METADATA_FILES = {"catalog.go", "catalog_test.go"}


class StageError(ValueError):
    """A safe, path-free staging failure."""


def _reparse(path):
    """Treat Windows junctions like links; a catalog must be a real tree."""
    try:
        return path.is_symlink() or path.is_junction()
    except OSError:
        # A failed reparse-point query is not safe evidence for traversal.
        return True


def _regular(path):
    return path.is_file() and not _reparse(path)


def _safe_member(name, descriptor="plugin.json"):
    if not isinstance(name, str) or not name or "\\" in name or ":" in name:
        return False
    member = PurePosixPath(name)
    return not member.is_absolute() and "." not in member.parts and ".." not in member.parts and str(member) == name and name != descriptor


def _digest(path):
    digest = hashlib.sha256()
    with path.open("rb") as source:
        for part in iter(lambda: source.read(1024 * 1024), b""):
            digest.update(part)
    return digest.hexdigest()


def _read_package(directory, descriptor="plugin.json"):
    if _reparse(directory) or not directory.is_dir() or not IDENTIFIER.fullmatch(directory.name):
        raise StageError("invalid_plugin_directory")
    manifest_path = directory / descriptor
    if not _regular(manifest_path):
        raise StageError("plugin_manifest_missing")
    try:
        manifest = json.loads(manifest_path.read_text(encoding="utf8"))
        indexed = manifest["files"]
    except (OSError, UnicodeError, ValueError, KeyError, TypeError):
        raise StageError("invalid_plugin_manifest") from None
    if manifest.get("id") != directory.name or not isinstance(indexed, dict) or not indexed:
        raise StageError("invalid_plugin_manifest")
    expected = {descriptor}
    for member, digest in indexed.items():
        if not _safe_member(member, descriptor) or not isinstance(digest, str) or not re.fullmatch(r"[0-9a-f]{64}", digest):
            raise StageError("invalid_plugin_manifest")
        path = directory.joinpath(*PurePosixPath(member).parts)
        if not _regular(path) or _digest(path) != digest:
            raise StageError("plugin_manifest_hash_mismatch")
        expected.add(member)
    found = set()
    for current, directories, files in os.walk(directory, followlinks=False):
        root = Path(current)
        if _reparse(root) or any(_reparse(root / child) for child in directories + files):
            raise StageError("plugin_links_forbidden")
        for filename in files:
            path = root / filename
            if not _regular(path):
                raise StageError("plugin_special_file_forbidden")
            found.add(path.relative_to(directory).as_posix())
    if found != expected:
        raise StageError("plugin_contains_unindexed_files")
    return sorted(expected)


def _catalog(source, descriptor="plugin.json", selected=None):
    if _reparse(source) or not source.is_dir():
        raise StageError("source_catalog_unavailable")
    entries = list(source.iterdir())
    if not entries:
        raise StageError("source_catalog_empty")
    directories = []
    for entry in entries:
        if _reparse(entry):
            raise StageError("source_catalog_contains_non_package")
        if entry.is_dir():
            directories.append(entry)
            continue
        if not _regular(entry) or entry.name not in ROOT_METADATA_FILES:
            raise StageError("source_catalog_contains_non_package")
    if not directories:
        raise StageError("source_catalog_empty")
    if selected is not None:
        requested = {selected} if isinstance(selected, str) else set(selected)
        directories = [directory for directory in directories if directory.name in requested]
        if not directories or len(directories) != len(requested):
            raise StageError("requested_package_unavailable")
    packages = {}
    for directory in sorted(directories, key=lambda value: value.name):
        packages[directory.name] = _read_package(directory, descriptor)
    return packages


def _same_file(left, right):
    if not _regular(left) or not _regular(right) or left.stat().st_size != right.stat().st_size:
        return False
    return _digest(left) == _digest(right)


def _existing_matches(source, output, packages):
    if _reparse(output) or not output.is_dir():
        return False
    expected = set()
    for package, members in packages.items():
        expected.update(f"{package}/{member}" for member in members)
    found = set()
    for current, directories, files in os.walk(output, followlinks=False):
        root = Path(current)
        if _reparse(root) or any(_reparse(root / child) for child in directories + files):
            return False
        for filename in files:
            path = root / filename
            if not _regular(path):
                return False
            found.add(path.relative_to(output).as_posix())
    if found != expected:
        return False
    return all(_same_file(source / relative, output / relative) for relative in expected)


def _atomic_copy(source, destination):
    """Publish one verified member without exposing a partial member file."""
    destination.parent.mkdir(parents=True, exist_ok=True)
    handle, temporary = tempfile.mkstemp(prefix=".stage-plugin-", dir=destination.parent)
    temporary = Path(temporary)
    try:
        with os.fdopen(handle, "wb") as staged, source.open("rb") as incoming:
            shutil.copyfileobj(incoming, staged)
            staged.flush()
            os.fsync(staged.fileno())
        os.replace(temporary, destination)
    except BaseException:
        temporary.unlink(missing_ok=True)
        raise


def _member_destination(output, package, member):
    """Return a member path only through real directories below output."""
    destination = output / package / Path(*PurePosixPath(member).parts)
    parent = output
    for part in (package, *PurePosixPath(member).parts[:-1]):
        parent /= part
        if parent.exists() and _reparse(parent):
            raise StageError("catalog_reparse_point_forbidden")
    return destination


def _inside_output(output, candidate):
    """Check a resolved deletion target remains a direct catalog member."""
    try:
        candidate.resolve(strict=False).relative_to(output.resolve(strict=True))
        return True
    except (OSError, ValueError):
        return False


def _manifest_members(path, package, descriptor="plugin.json"):
    if not _regular(path):
        raise StageError("output_catalog_unknown_intermediate_state")
    try:
        manifest = json.loads(path.read_text(encoding="utf8"))
        indexed = manifest["files"]
    except (OSError, UnicodeError, ValueError, KeyError, TypeError):
        raise StageError("output_catalog_unknown_intermediate_state") from None
    if manifest.get("id") != package or not isinstance(indexed, dict) or not indexed:
        raise StageError("output_catalog_unknown_intermediate_state")
    members = {descriptor}
    for member, digest in indexed.items():
        if not _safe_member(member, descriptor) or not isinstance(digest, str) or not re.fullmatch(r"[0-9a-f]{64}", digest):
            raise StageError("output_catalog_unknown_intermediate_state")
        members.add(member)
    return members, indexed


def _recoverable_existing_catalog(source, output, packages, descriptor="plugin.json"):
    """Recognize only the interrupted state created by this updater.

    A power or disk failure can happen after an atomic member replacement but
    before its package manifest.  The old manifest is still available, so a
    retry may continue only when every on-disk member is indexed by either that
    manifest or the fully validated new source catalog.  Any other state is
    refused rather than cleaning or guessing about user files.
    """
    if _reparse(output) or not output.is_dir():
        raise StageError("output_catalog_unknown_intermediate_state")
    old = {}
    for entry in output.iterdir():
        if _reparse(entry) or not entry.is_dir() or not IDENTIFIER.fullmatch(entry.name):
            raise StageError("output_catalog_unknown_intermediate_state")
        members, indexed = _manifest_members(entry / descriptor, entry.name, descriptor)
        if entry.name not in packages:
            raise StageError("catalog_package_delete_requires_fresh_directory")
        allowed = members | set(packages[entry.name])
        found = set()
        for current, directories, files in os.walk(entry, followlinks=False):
            root = Path(current)
            if _reparse(root) or any(_reparse(root / child) for child in directories + files):
                raise StageError("output_catalog_unknown_intermediate_state")
            for filename in files:
                path = root / filename
                if not _regular(path):
                    raise StageError("output_catalog_unknown_intermediate_state")
                member = path.relative_to(entry).as_posix()
                if member not in allowed:
                    raise StageError("output_catalog_unknown_intermediate_state")
                if member != descriptor:
                    accepted = {indexed.get(member)}
                    source_member = source / entry.name / Path(*PurePosixPath(member).parts)
                    if member in packages[entry.name] and _regular(source_member):
                        accepted.add(_digest(source_member))
                    if _digest(path) not in accepted:
                        raise StageError("output_catalog_unknown_intermediate_state")
                    found.add(member)
                else:
                    found.add(member)
        if descriptor not in found:
            raise StageError("output_catalog_unknown_intermediate_state")
        old[entry.name] = sorted(members)
    return old


def _update_existing(source, output, packages, descriptor="plugin.json"):
    """Replace indexed members only; package manifests are always published last."""
    if _reparse(output) or not output.is_dir():
        raise StageError("output_requires_existing_catalog")
    try:
        old = _catalog(output, descriptor)  # Validate every old member before planning any write.
        if _existing_matches(source, output, packages):
            return {
                "packages": sorted(packages),
                "files": sum(len(items) for items in packages.values()),
                "idempotent": True,
                "updated": True,
            }
    except StageError:
        # Do not accept arbitrary damaged output.  This path is only for a
        # previous interrupted --update-existing invocation; validate its exact
        # old-or-new indexed layout before allowing an in-place retry.
        old = _recoverable_existing_catalog(source, output, packages, descriptor)
    removed_packages = set(old) - set(packages)
    if removed_packages:
        # A package root may contain user state in a future layout.  Do not infer
        # that it is safe to delete merely because its source package vanished.
        raise StageError("catalog_package_delete_requires_fresh_directory")

    plan = []
    for package, members in packages.items():
        old_members = set(old.get(package, ()))
        new_members = set(members)
        for member in sorted(new_members - {descriptor}):
            plan.append(("copy", package, member))
        for member in sorted(old_members - new_members):
            # _catalog above proves this was an indexed regular file.  Retain an
            # explicit resolved-root check at the destructive operation.
            target = _member_destination(output, package, member)
            if not _inside_output(output, target):
                raise StageError("unsafe_obsolete_catalog_member")
            plan.append(("unlink", package, member))
        # Plugin manifest is deliberately last for this package: it is the
        # declaration which makes copied members live.
        plan.append(("manifest", package, descriptor))

    for action, package, member in plan:
        destination = _member_destination(output, package, member)
        if action == "copy":
            _atomic_copy(source / package / Path(*PurePosixPath(member).parts), destination)
        elif action == "unlink":
            if not _regular(destination) or not _inside_output(output, destination):
                raise StageError("unsafe_obsolete_catalog_member")
            destination.unlink()
        else:
            _atomic_copy(source / package / descriptor, destination)
    return {
        "packages": sorted(packages),
        "files": sum(len(items) for items in packages.values()),
        "idempotent": False,
        "updated": True,
    }


def _overlaps(source, output):
    """Return whether either catalog path contains the other, without writing."""
    try:
        source_resolved = source.resolve(strict=False)
        output_resolved = output.resolve(strict=False)
        output_resolved.relative_to(source_resolved)
        return True
    except ValueError:
        try:
            source_resolved.relative_to(output_resolved)
            return True
        except ValueError:
            return False


def stage(source, output, update_existing=False, descriptor="plugin.json", selected=None):
    """Validate source catalog then create it once, or prove existing equality."""
    if descriptor not in {"plugin.json", "app.json"}:
        raise StageError("unsupported_package_descriptor")
    # Do not resolve here: resolving the catalog root would hide a symlink
    # before the no-links validation below can reject it.
    source, output = Path(source).absolute(), Path(output).absolute()
    if _reparse(source):
        raise StageError("source_catalog_unavailable")
    if output.exists() and _reparse(output):
        raise StageError("output_requires_fresh_directory")
    # A nested output would become an unindexed source member during a later
    # invocation. Reject all equal/ancestor relations before mkdir/copyfile.
    if _overlaps(source, output):
        raise StageError("source_output_overlap")
    packages = _catalog(source, descriptor, selected)
    if output.exists():
        if _reparse(output) or not output.is_dir():
            raise StageError("output_requires_fresh_directory")
        if any(output.iterdir()):
            if update_existing:
                return _update_existing(source, output, packages, descriptor)
            if not _existing_matches(source, output, packages):
                raise StageError("output_requires_fresh_directory")
            return {"packages": sorted(packages), "files": sum(len(items) for items in packages.values()), "idempotent": True}
    output.mkdir(parents=True, exist_ok=True)
    for package, members in packages.items():
        for member in members:
            destination = _member_destination(output, package, member)
            destination.parent.mkdir(parents=True, exist_ok=True)
            shutil.copyfile(source / package / Path(*PurePosixPath(member).parts), destination)
    return {"packages": sorted(packages), "files": sum(len(items) for items in packages.values()), "idempotent": False}


def main(argv=None):
    parser = argparse.ArgumentParser(description="stage verified official plugin source catalog")
    parser.add_argument("--source", required=True, help="official plugin source directory, for example plugins")
    parser.add_argument("--output", required=True, help="new catalog directory, for example OUTPUT/plugins")
    parser.add_argument("--update-existing", action="store_true", help="replace only verified indexed members in an existing catalog")
    parser.add_argument("--descriptor", choices=("plugin.json", "app.json"), default="plugin.json", help="package descriptor filename")
    parser.add_argument("--package", action="append", help="stage only the named packages (repeat for each)")
    args = parser.parse_args(argv)
    try:
        result = stage(args.source, args.output, update_existing=args.update_existing, descriptor=args.descriptor, selected=args.package)
    except StageError as error:
        print(str(error), file=sys.stderr)
        return 1
    print(json.dumps(result, ensure_ascii=False, sort_keys=True))
    return 0


if __name__ == "__main__":
    sys.exit(main())
