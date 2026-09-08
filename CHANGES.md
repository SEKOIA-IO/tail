# Version v1.6.5

* Fix the tailing of files that are not UTF-8 encoded. `transform.Reader`, used
to decode them, latches the first error of the underlying reader: once a
followed file reached EOF, its decoder was "complete" and never read the file
again, so everything appended afterwards was silently ignored until the file was
reopened. Decoding is now done by a resumable reader that keeps its pending
bytes across EOF.
* Drop the byte order mark of a file instead of decoding it as an U+FEFF
character prepended to its first line. When it disagrees with the configured
encoding, the mark now also corrects the endianness.
* Keep decoding after a seek. Seeking used to reset the buffered reader on the
file itself, dropping the decoder, so every line read afterwards was made of raw
undecoded bytes.
* Only feed the bytes actually read to the encoding detection. The rest of the
1024 bytes buffer was NUL padding, which the detector confidently reports as
UTF-32: a freshly rotated UTF-16 file, which only contains a byte order mark,
was detected as UTF-32LE and decoded as garbage. Detection is now also skipped
when the sample is too small to conclude anything, and its result is memoized
until the file is reopened.
* Stop seeking to the end of the file after an incomplete line has been read.
The seek was either a no-op, when nothing had been appended since the read, or
a silent loss of everything the writer appended in between. The tailing now
simply resumes where the incomplete line ends.
* Retry the encoding detection when it could not conclude, instead of reading
the file as UTF-8 until it is reopened. A file that is empty when it is opened
cannot be identified, so its first lines used to be shipped as raw undecoded
bytes. The detection is now retried when the file grows, and it settles for good
once a full sample has been examined.
* Remove the position lookup that was performed, and discarded, before every
line, which costs a system call per line.
* Stop reporting a file that cannot be opened as a deleted file on Windows.
Since the polling watcher stats through an open handle, an open that fails
because a writer uses a restrictive sharing mode, or because an antivirus or a
backup holds the file for a moment, was read as a deletion: the tail reopened
the file and shipped its whole content again. The directory entry is now
checked before concluding anything, and the failures that remain go through the
existing tolerance of consecutive polling errors.
* Report the position of the consumer in the file for every line, whatever the
encoding of the file is. `SeekInfo.Offset` used to be computed by subtracting a
number of decoded bytes from the position of the file, and it ignored the bytes
held by the decoder, so it was meaningless for a file that is not UTF-8 encoded:
persisting it and seeking back to it on the next start would skip data, or land
in the middle of a character. The decoding reader now splits the lines itself
and replays the decoding of what it returns, through a second decoder that lags
behind, which gives the exact number of source bytes each line was made of.
* Small cleanups: unreachable statements in the watchers, and `io/ioutil`.

# Version v1.4.9
* Bump fsnotify to v1.5.1 fixes issue #28, hpcloud/tail#90.
* PR #27: "Add timeout to tests"by @kokes++. Also timeout on FreeBSD.
* PR #29: "Use temp directory for tests, instead of relative" by @ches++.

# Version v1.4.7-v1.4.8
* Documentation updates.
* Small linter cleanups.
* Added example in test.

# Version v1.4.6

* Document the usage of Cleanup when re-reading a file (thanks to @lesovsky) for issue #18.
* Add example directories with example and tests for issues.

# Version v1.4.4-v1.4.5

* Fix of checksum problem because of forced tag. No changes to the code.

# Version v1.4.1

* Incorporated PR 162 by by Mohammed902: "Simplify non-Windows build tag".

# Version v1.4.0

* Incorporated PR 9 by mschneider82: "Added seekinfo to Tail".

# Version v1.3.1

* Incorporated PR 7: "Fix deadlock when stopping on non-empty file/buffer",
fixes upstream issue 93.


# Version v1.3.0

* Incorporated changes of unmerged upstream PR 149 by mezzi: "added line num
to Line struct".

# Version v1.2.1

* Incorporated changes of unmerged upstream PR 128 by jadekler: "Compile-able
code in readme".
* Incorporated changes of unmerged upstream PR 130 by fgeller: "small change
to comment wording".
* Incorporated changes of unmerged upstream PR 133 by sm3142: "removed
spurious newlines from log messages".

# Version v1.2.0

* Incorporated changes of unmerged upstream PR 126 by Code-Hex: "Solved the
 problem for never return the last line if it's not followed by a newline".
* Incorporated changes of unmerged upstream PR 131 by StoicPerlman: "Remove
deprecated os.SEEK consts". The changes bumped the minimal supported Go
release to 1.9.

# Version v1.1.0

* migration to go modules.
* release of master branch of the dormant upstream, because it contains
fixes and improvement no present in the tagged release.
