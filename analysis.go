package tar_diff

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha1"
	"encoding/hex"
	"io"
	"io/ioutil"
	"log"
	"os"
	"path"
	"sort"
	"strings"
)

const (
	similarityPercentThreshold = 30
)

type TarFileInfo struct {
	index         int
	basename      string
	path          string
	size          int64
	sha1          string
	isExecutable  bool
	worldReadable bool
	blobs         []RollsumBlob
}

type TarInfo struct {
	files []TarFileInfo // Sorted by size, no size=0 files
}

type TargetInfo struct {
	file           *TarFileInfo
	source         *SourceInfo
	rollsumMatches *RollsumMatches
}

type SourceInfo struct {
	file         *TarFileInfo
	usedForDelta bool
	offset       int64
}

type DeltaAnalysis struct {
	targetInfos       []TargetInfo
	sourceInfos       []SourceInfo
	sourceData        *os.File
	targetInfoByIndex map[int]*TargetInfo
}

func (a *DeltaAnalysis) Close() {
	a.sourceData.Close()
	os.Remove(a.sourceData.Name())
}

func isSparseFile(hdr *tar.Header) bool {
	if hdr.Typeflag == tar.TypeGNUSparse {
		return true
	}
	if hdr.Typeflag == tar.TypeReg &&
		(hdr.PAXRecords["GNU.sparse.major"] != "" || hdr.PAXRecords["GNU.sparse.minor"] != "" || hdr.PAXRecords["GNU.sparse.map"] != "") {
		return true
	}

	return false
}

func useTarHeader(hdr *tar.Header) bool {
	if hdr.Typeflag != tar.TypeReg {
		return false
	}

	// We never create file info for empty files, since we can't delta with them
	if hdr.Size == 0 {
		return false
	}

	// Sparse headers will return file content that doesn't match the tarfile stream contents, so lets just
	// not delta them. We could do better here, but I don't think sparse files are very common.
	if isSparseFile(hdr) {
		return false
	}

	return true
}

func analyzeTar(targzFile io.Reader) (*TarInfo, error) {
	tarFile, err := gzip.NewReader(targzFile)
	if err != nil {
		return nil, err
	}
	defer tarFile.Close()

	files := make([]TarFileInfo, 0)

	rdr := tar.NewReader(tarFile)
	for index := 0; true; index++ {
		var hdr *tar.Header
		hdr, err = rdr.Next()
		if err != nil {
			if err == io.EOF {
				break // Expected error
			} else {
				return nil, err
			}
		}
		if useTarHeader(hdr) {
			h := sha1.New()
			r := NewRollsum()
			w := io.MultiWriter(h, r)
			if _, err := io.Copy(w, rdr); err != nil {
				return nil, err
			}
			blobs := r.GetBlobs()

			header := r.GetHeader()

			isExecutable := false
			// Check for elf header
			if len(header) > 4 && header[0] == 0x7f && header[1] == 'E' && header[2] == 'L' && header[3] == 'F' {
				isExecutable = true
			}

			worldReadable := false
			if (hdr.Mode & 00004) != 0 {
				worldReadable = true
			}

			last := int64(0)
			for i := range blobs {
				blob := blobs[i]
				// Do some internal self validation
				if blob.offset != last {
					log.Fatalf("Internal error: Wrong blob start")
				}
				if blob.size > maxBlobSize {
					log.Fatalf("Internal error: Wrong blob size")
				}
				last = blob.offset + blob.size
			}
			if last != hdr.Size {
				log.Fatalf("Internal error: Wrong blob end")
			}

			fileInfo := TarFileInfo{
				index:         index,
				basename:      path.Base(hdr.Name),
				path:          hdr.Name,
				size:          hdr.Size,
				sha1:          hex.EncodeToString(h.Sum(nil)),
				isExecutable:  isExecutable,
				worldReadable: worldReadable,
				blobs:         blobs,
			}
			files = append(files, fileInfo)
		}
	}

	// Sort, smallest files first
	sort.Slice(files, func(i, j int) bool {
		return files[i].size < files[j].size
	})

	info := TarInfo{files: files}
	return &info, nil
}

func isDeltaCandidate(file *TarFileInfo) bool {
	// We don't want to delta files that may be problematic to
	// read (e.g. /etc/shadow) when applying the delta. These are
	// uncommon anyway so no big deal.
	if !file.worldReadable {
		return false
	}

	// Look for known non-delta-able files (currently just compression)
	// NB: We explicitly don't have .gz here in case someone might be
	// using --rsyncable for that.
	if strings.HasPrefix(file.basename, ".xz") ||
		strings.HasPrefix(file.basename, ".bz2") {
		return false
	}

	return true
}

func nameIsSimilar(a *TarFileInfo, b *TarFileInfo, fuzzy int) bool {
	if fuzzy == 0 {
		return a.basename == b.basename
	} else {
		aa := strings.SplitAfterN(a.basename, ".", 2)[0]
		bb := strings.SplitAfterN(b.basename, ".", 2)[0]
		return aa == bb
	}
}

func extractDeltaData(tarGzFile io.Reader, sourceByPath map[string]*SourceInfo, dest *os.File) error {
	offset := int64(0)

	tarFile, err := gzip.NewReader(tarGzFile)
	if err != nil {
		return err
	}
	defer tarFile.Close()

	rdr := tar.NewReader(tarFile)
	for {
		var hdr *tar.Header
		hdr, err = rdr.Next()
		if err != nil {
			if err == io.EOF {
				break // Expected error
			} else {
				return err
			}
		}
		if useTarHeader(hdr) {
			info := sourceByPath[hdr.Name]
			if info.usedForDelta {
				info.offset = offset
				offset += hdr.Size
				if _, err := io.Copy(dest, rdr); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func analyzeForDelta(old *TarInfo, new *TarInfo, oldFile io.Reader) (*DeltaAnalysis, error) {
	sourceInfos := make([]SourceInfo, 0, len(old.files))
	for i := range old.files {
		sourceInfos = append(sourceInfos, SourceInfo{file: &old.files[i]})
	}

	sourceBySha1 := make(map[string]*SourceInfo)
	sourceByPath := make(map[string]*SourceInfo)
	for i := range sourceInfos {
		s := &sourceInfos[i]
		sourceBySha1[s.file.sha1] = s
		sourceByPath[s.file.path] = s
	}

	targetInfos := make([]TargetInfo, 0, len(new.files))

	for i := range new.files {
		file := &new.files[i]
		// First look for exact content match
		usedForDelta := false
		var source *SourceInfo
		sha1Source := sourceBySha1[file.sha1]
		// If same sha1 and size, use original total size
		if sha1Source != nil && file.size == sha1Source.file.size && sha1Source.file.worldReadable {
			source = sha1Source
		}
		if source == nil && isDeltaCandidate(file) {
			// No exact match, try to find a useful source

			// If size is vastly different not useful to delta
			minSize := file.size - file.size*similarityPercentThreshold/100
			maxSize := file.size + file.size*similarityPercentThreshold/100

			// First check by exact pathname match
			s := sourceByPath[file.path]

			if s != nil && isDeltaCandidate(s.file) && s.file.size >= minSize && s.file.size < maxSize {
				usedForDelta = true
				source = s
			} else {
				// Check for moved (first) or renamed (second) versions
				lower := 0
				upper := len(sourceInfos)
				for fuzzy := 0; fuzzy < 2 && source == nil; fuzzy++ {
					for j := lower; j < upper; j++ {
						s = &sourceInfos[j]
						if !isDeltaCandidate(s.file) {
							continue
						}

						if s.file.size < minSize {
							lower++
							continue
						}

						if s.file.size > maxSize {
							break
						}

						if !nameIsSimilar(file, s.file, fuzzy) {
							continue
						}

						usedForDelta = true
						source = s
						break
					}
				}
			}
		}

		var rollsumMatches *RollsumMatches
		if source != nil {
			source.usedForDelta = usedForDelta

			if usedForDelta {
				rollsumMatches = ComputeRollsumMatches(source.file.blobs, file.blobs)
			}
		}
		info := TargetInfo{file: file, source: source, rollsumMatches: rollsumMatches}
		targetInfos = append(targetInfos, info)
	}

	targetInfoByIndex := make(map[int]*TargetInfo)
	for i := range targetInfos {
		t := &targetInfos[i]
		targetInfoByIndex[t.file.index] = t
	}

	tmpfile, err := ioutil.TempFile("/var/tmp", "tar-diff-")
	if err != nil {
		return nil, err
	}

	err = extractDeltaData(oldFile, sourceByPath, tmpfile)
	if err != nil {
		return nil, err
	}

	return &DeltaAnalysis{targetInfos: targetInfos, targetInfoByIndex: targetInfoByIndex, sourceInfos: sourceInfos, sourceData: tmpfile}, nil
}
