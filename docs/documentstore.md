# Document storage contract

`documentstore` provides checked, durable replacement of plugin configuration
files on Linux. Open an existing real directory with `documentstore.Open`, then
use root-relative canonical paths. Paths may name files in existing nested
directories, but absolute paths, traversal, redundant path components, and
symlinked parents or targets are rejected. Basenames beginning with
`.documentstore-` are reserved for internal lock and temporary files.
The root and every traversed parent directory must be controlled by trusted
principals and must not be writable by untrusted users. Entry or sidecar
manipulation in an untrusted-writable directory is outside this security model.

`Read` returns the exact bytes and a SHA-256 content revision. `Write` accepts
the revision that the caller read and returns `documentstore.ErrConflict` if
the file has changed. Passing the zero revision performs checked creation and
conflicts if the file already exists. Existing regular files retain their mode,
UID, and GID; newly created documents are owner-only (`0600`). Special files
and symlinks are not documents.

Hard-linked regular files are accepted. A read observes the shared inode. A
successful write atomically replaces only the path beneath the store root, so
other links retain the prior bytes while the replacement inherits the prior
file's mode, UID, and GID. Writes through another link are non-cooperating and
fall under the race limitation below.

Readers take a shared `flock` and writers take an exclusive `flock` on a
same-directory sidecar named `.documentstore-lock-<sha256-basename>`.
Cooperating processes therefore serialize the revision check, temporary-file
write, and rename, and the writer holds its lock through publication. Writers
that bypass this package do not take that lock: they can still race the final
rename or change the file immediately afterward. The API does not claim
protection from non-cooperating external writers.

A successful write creates an owner-only temporary file in the destination
directory, writes it, applies preserved ownership and full mode when replacing
a file, fsyncs it, atomically renames it over the destination, and fsyncs the
directory. Pre-publication failure or cancellation removes the temporary file;
if cleanup itself fails, that failure is included in the returned error.
If directory fsync fails after rename, `Write` returns both the committed
revision and an error: the new bytes are visible, but crash durability was not
established. Cancellation is observed before work, while waiting for a lock,
and immediately before rename. Once rename succeeds, cancellation cannot roll
back publication.

Call `Close` to release the open root directory. It waits for active operations;
later operations return `documentstore.ErrClosed`. Other platforms expose the
same API but return `documentstore.ErrUnsupported`.
