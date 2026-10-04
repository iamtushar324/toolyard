"""Same-repository source resolution and private immutable build contexts.

GitHub metadata is read-only. Fetching/archiving runs only when explicitly called.
Branch code is never used as manager configuration or Kubernetes manifests.
"""
from dataclasses import asdict, dataclass
import hashlib
import io
import json
from pathlib import Path, PurePosixPath
import re
import subprocess
import tarfile
from urllib.parse import quote

REPOSITORY = "beknown-work/beknown-services"
SHA = re.compile(r"[0-9a-f]{40}\Z")
MAX_SOURCE_BYTES = 256 * 1024 * 1024
MAX_FILES = 30000


class CredentialPath(ValueError):
    pass


@dataclass(frozen=True)
class Source:
    repository: str
    reference: str
    commit_sha: str
    branch: str
    pull_request: int | None = None
    draft: bool = False


def github_read(endpoint):
    completed = subprocess.run(["gh", "api", endpoint], capture_output=True, timeout=30, check=False)
    if completed.returncode or len(completed.stdout) > 1024 * 1024:
        raise RuntimeError("GitHub source lookup failed; no preview was submitted")
    return json.loads(completed.stdout)


def resolve_source(reference, read=github_read):
    if not isinstance(reference, str) or len(reference) > 210:
        raise ValueError("Use branch:<name>, pr:<number> or commit:<40hex>")
    kind, separator, value = reference.partition(":")
    if not separator:
        raise ValueError("A typed source reference is required")
    if kind == "pr":
        if not re.fullmatch(r"[1-9][0-9]{0,7}", value):
            raise ValueError("Invalid PR number")
        result = read(f"repos/{REPOSITORY}/pulls/{value}")
        if (result.get("base", {}).get("repo", {}).get("full_name") != REPOSITORY
                or result.get("head", {}).get("repo", {}).get("full_name") != REPOSITORY):
            raise PermissionError("Fork PRs require a separate approved trust profile")
        source = Source(REPOSITORY, reference, result["head"]["sha"], result["head"]["ref"], int(value), result.get("draft") is True)
    elif kind == "branch":
        if (not re.fullmatch(r"[A-Za-z0-9][A-Za-z0-9._/-]{0,199}", value)
                or ".." in value or "//" in value or value.endswith(("/", ".", ".lock"))):
            raise ValueError("Invalid branch name")
        result = read(f"repos/{REPOSITORY}/branches/{quote(value, safe='')}")
        if result.get("name") != value:
            raise ValueError("GitHub branch identity mismatch")
        source = Source(REPOSITORY, reference, result["commit"]["sha"], value)
    elif kind == "commit" and SHA.fullmatch(value):
        result = read(f"repos/{REPOSITORY}/commits/{value}")
        if result.get("sha") != value:
            raise ValueError("GitHub commit identity mismatch")
        source = Source(REPOSITORY, reference, value, "commit/" + value)
    else:
        raise ValueError("Use branch:<name>, pr:<number> or commit:<40hex>")
    if not SHA.fullmatch(source.commit_sha) or not isinstance(source.branch, str):
        raise ValueError("GitHub did not return an immutable source identity")
    return source


def allowed_path(name):
    path = PurePosixPath(name)
    if path.is_absolute() or ".." in path.parts or not path.parts or "\\" in name:
        raise ValueError("Unsafe source archive path")
    for part in path.parts:
        lower = part.lower()
        if (lower == ".git" or lower.startswith(".env") or lower in {"id_rsa", "id_ed25519", "credentials", ".aws"}
                or lower.endswith((".pem", ".key", ".p12", ".pfx"))):
            raise CredentialPath("Credential-shaped paths cannot enter a build context")
    return path


def normalize_archive(raw, overlays=None):
    """Deterministic archive, with traversal/device/link and size bounds.

    overlays are explicit path->bytes or None deletions from a trusted local CLI.
    MCP requests cannot submit arbitrary archives or patch paths.
    """
    if type(raw) is not bytes or len(raw) > MAX_SOURCE_BYTES:
        raise ValueError("Source archive size exceeded")
    files, links, total = {}, {}, 0
    with tarfile.open(fileobj=io.BytesIO(raw), mode="r:") as archive:
        for index, item in enumerate(archive):
            if index >= MAX_FILES * 2 or len(links) + len(files) >= MAX_FILES:
                raise ValueError("Source entry count exceeded")
            try:
                path = allowed_path(item.name.rstrip("/"))
            except CredentialPath:
                # Includes tracked .env examples. The trusted recipe excludes every
                # credential-shaped path instead of silently accepting sample secrets.
                continue
            name = str(path)
            if name in files or name in links:
                raise ValueError("Duplicate source archive path")
            if item.isdir():
                continue
            if item.issym():
                target = PurePosixPath(item.linkname)
                if target.is_absolute() or ".." in target.parts:
                    raise ValueError("External source symlinks are refused")
                allowed_path(str(path.parent / target))
                links[name] = item.linkname
                continue
            if not item.isfile() or item.size < 0 or item.size > MAX_SOURCE_BYTES:
                raise ValueError("Only bounded regular source files are accepted")
            total += item.size
            if total > MAX_SOURCE_BYTES or len(files) + len(links) >= MAX_FILES:
                raise ValueError("Source context bounds exceeded")
            files[name] = (archive.extractfile(item).read(), item.mode & 0o777)
    for name, content in (overlays or {}).items():
        allowed_path(name)
        if name in links:
            raise ValueError("Uncommitted symlink changes need an explicit supported snapshot format")
        if content is None:
            files.pop(name, None)
        elif type(content) is bytes:
            files[name] = (content, files.get(name, (None, 0o644))[1])
        elif (isinstance(content, tuple) and len(content) == 2 and type(content[0]) is bytes
              and type(content[1]) is int and content[1] in {0o644, 0o755}):
            files[name] = content
        else:
            raise ValueError("Invalid source overlay")
    if sum(len(value[0]) for value in files.values()) > MAX_SOURCE_BYTES or len(files) + len(links) > MAX_FILES:
        raise ValueError("Source overlay bounds exceeded")
    for name in files:
        if any(parent.as_posix() in links for parent in PurePosixPath(name).parents):
            raise ValueError("Source files cannot be nested below symlinks")
    output = io.BytesIO()
    with tarfile.open(fileobj=output, mode="w", format=tarfile.PAX_FORMAT) as archive:
        for name in sorted(set(files) | set(links)):
            item = tarfile.TarInfo(name)
            item.mtime = 0
            if name in links:
                item.type, item.linkname, item.mode = tarfile.SYMTYPE, links[name], 0o777
                archive.addfile(item)
            else:
                content, item.mode = files[name]
                item.size = len(content)
                archive.addfile(item, io.BytesIO(content))
    return output.getvalue()


def local_snapshot(worktree, source, output, scan, include_uncommitted=False, untracked=()):
    """Prepare only. scan must be a trusted redacting secret scanner that fails closed.

    Output must be a new private file outside the checkout. Untracked files require
    an explicit list; .env and credential-shaped files are always refused.
    """
    import os
    root, output = Path(worktree).resolve(), Path(output).absolute()
    if any(parent.is_symlink() for parent in (output, *output.parents)):
        raise ValueError("Snapshot staging cannot traverse symlinks")
    output = output.resolve()
    if output == root or root in output.parents:
        raise ValueError("Snapshot staging must be outside the source checkout")
    def git(arguments):
        result = subprocess.run(["git", "-C", str(root)] + arguments, capture_output=True, timeout=30)
        if result.returncode:
            raise RuntimeError("Source snapshot Git operation failed")
        return result.stdout
    if git(["rev-parse", source.commit_sha + "^{commit}"]).decode().strip() != source.commit_sha:
        raise ValueError("Source commit is absent from this checkout")
    overlays = {}
    if include_uncommitted:
        paths = git(["diff", "--name-only", "-z", source.commit_sha]).decode().split("\0")
        paths += list(untracked)
        if len(paths) > MAX_FILES:
            raise ValueError("Too many source changes")
        for name in filter(None, paths):
            allowed_path(name)
            path = root / name
            if path.is_symlink() or any(parent.is_symlink() for parent in path.parents if parent != root and root in parent.parents):
                raise ValueError("Uncommitted symlinks are refused")
            if path.exists():
                if not path.is_file() or path.stat().st_size > MAX_SOURCE_BYTES:
                    raise ValueError("Unsupported uncommitted source file")
                overlays[name] = (path.read_bytes(), 0o755 if path.stat().st_mode & 0o111 else 0o644)
            else:
                overlays[name] = None
    elif untracked:
        raise ValueError("Untracked snapshots require explicit include_uncommitted")
    archive = normalize_archive(git(["archive", "--format=tar", source.commit_sha]), overlays)
    # The scanner receives bytes in memory or stages in its own private directory.
    # Never print scanner findings, since findings can contain credentials.
    if scan(archive) is not True:
        raise ValueError("Secret scan did not pass; snapshot was not saved")
    output.parent.mkdir(mode=0o700, parents=True, exist_ok=True)
    if output.parent.stat().st_mode & 0o077:
        raise ValueError("Snapshot parent must be private0700")
    directory = os.open(output.parent, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW)
    try:
        if os.fstat(directory).st_mode & 0o077:
            raise ValueError("Snapshot parent must be private0700")
        descriptor = os.open(output.name, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW,
                             0o600, dir_fd=directory)
        with os.fdopen(descriptor, "wb") as handle:
            handle.write(archive)
    finally:
        os.close(directory)
    return dict(asdict(source), archive_sha256=hashlib.sha256(archive).hexdigest(),
                includes_uncommitted=bool(include_uncommitted), changed_paths=sorted(overlays),
                secret_scan="passed", credential_paths_excluded=True, build_recipe="bks-arm64-v1")
