package compactv2

import (
	"archive/zip"
	"bytes"
	"encoding/binary"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"os"
	"strconv"

	"sean.lo/cachelib/common"
)

// Bundle linear size in tiles
//const _BSZ = 128

// Tiles per bundle
//const _BSZ2 = _BSZ * _BSZ

// Index size in bytes
//const _IDXSZ = _BSZ2 * 8

const _BUNDLX_MAXIDX = 128
const _COMPACT_CACHE_HEADER_LENGTH = 64
const _BUNDLE_EXT = ".bundle"

type TileCacheInfo struct {
	XMLName          xml.Name                `xml:"TileCacheInfo"`
	OriginX          float64                 `xml:"TileOrigin>X"`
	OriginY          float64                 `xml:"TileOrigin>Y"`
	TileRows         int16                   `xml:"TileRows"`
	TileColumns      int16                   `xml:"TileCols"`
	DPI              int16                   `xml:"DPI"`
	LODS             []common.LOD            `xml:"LODInfos>LODInfo"`
	SpatialReference common.SpatialReference `xml:"SpatialReference"`
	MaxLOD           int
	MinLOD           int
	MaxScale         float64
	MinScale         float64
}

type TileImageInfo struct {
	XMLName            xml.Name `xml:"TileImageInfo" json:"-"`
	CacheTileFormat    string   `xml:"CacheTileFormat" json:"-"`
	CompressionQuality float32  `xml:"CompressionQuality" json:"compressionQuality"`
	BandCount          int      `xml:"BandCount" json:"bandCount"`
	LERCError          float32  `xml:"LERCError" json:"lercError"`
	Format             string   `json:"format"`
}

type Tiles struct {
	XMLName       xml.Name      `xml:"CacheInfo"`
	TileCacheInfo TileCacheInfo `xml:"TileCacheInfo"`
	TileImageInfo TileImageInfo `xml:"TileImageInfo"`
	RootPath      string
	EnvelopeN     common.Extent
	isTPKX        bool
	fileIndex     map[string]int
	RootJSONFile  *RootJSONFile
	zipReader     *zip.ReadCloser
}

type Tilemap struct {
	Adjusted bool   `json:"adjusted"`
	Location string `json:"location"`
	Data     []int  `json:"data"`
}

type RootJSONFile struct {
	TileImageInfo   TileImageInfo           `json:"tileImageInfo"`
	Name            string                  `json:"name"`
	Version         float64                 `json:"version"`
	ServiceDesc     string                  `json:"serviceDescription"`
	TileBundlesPath string                  `json:"tileBundlesPath"`
	SpatialRef      common.SpatialReference `json:"spatialReference"`
	Units           string                  `json:"units"`
	MinLOD          int                     `json:"minLOD"`
	MaxLOD          int                     `json:"maxLOD"`
	MinScale        float64                 `json:"minScale"`
	MaxScale        float64                 `json:"maxScale"`
	Resampling      bool                    `json:"resampling"`
	TileInfo        common.TileInfo         `json:"tileInfo"`
	ExportAllowed   bool                    `json:"exportTilesAllowed"`
	StorageInfo     StorageInfo             `json:"storageInfo"`
	InitialExtent   common.Extent           `json:"initialExtent"`
	FullExtent      common.Extent           `json:"fullExtent"`
	Layers          []json.RawMessage       `json:"layers"`
}

type StorageInfo struct {
	StorageFormat string `json:"storageFormat"`
	PacketSize    int    `json:"packetSize"`
}

func (t *Tiles) Load(root string, minlod *int, maxlod *int, isCompact bool) {
	t.RootPath = root
	t.isTPKX = isCompact

	if t.isTPKX {
		r, err := zip.OpenReader(root)
		if err != nil {
			fmt.Printf("无法打开文件: %v", err)
			return
		} else {
			t.fileIndex = make(map[string]int, len(r.File))
			for i, f := range r.File {
				t.fileIndex[f.Name] = i
			}

			r.Close()

			// 读取内容
			data, success := t.openFile("root.json")
			if !success {
				fmt.Printf("读取 root.json 失败: %v", err)
				return
			}

			// 反序列化 JSON
			//var config TileVectorService
			t.RootJSONFile = &RootJSONFile{}
			if err := json.Unmarshal(data, t.RootJSONFile); err != nil {
				fmt.Printf("解析root失败: %v", err)
				return
			}
		}

	} else {
		xmlFile, err := os.Open(root + string(os.PathSeparator) + "Conf.xml")
		if err != nil {
			xmlFile, err = os.Open(root + string(os.PathSeparator) + "conf.xml")
			if err != nil {
				fmt.Println("无法打开Conf.xml 或者conf.xml")
			}
		}
		defer xmlFile.Close()
		//fmt.Println("Successfully Opened conf.xml")
		byteValue, _ := io.ReadAll(xmlFile)
		e := xml.Unmarshal([]byte(byteValue), &t)
		fmt.Print(e)

		if minlod != nil {
			t.TileCacheInfo.MinLOD = *minlod
			t.TileCacheInfo.MaxLOD = *maxlod
		} else {
			files, _ := os.ReadDir(root + string(os.PathSeparator) + "_alllayers")
			first := files[0].Name()
			last := files[len(files)-1].Name()
			t.TileCacheInfo.MinLOD, _ = strconv.Atoi(first[1:])
			t.TileCacheInfo.MaxLOD, _ = strconv.Atoi(last[1:])
		}

		t.TileCacheInfo.MinScale = t.TileCacheInfo.LODS[t.TileCacheInfo.MinLOD].Scale
		t.TileCacheInfo.MaxScale = t.TileCacheInfo.LODS[t.TileCacheInfo.MaxLOD].Scale

		cdiFile, err := os.Open(root + string(os.PathSeparator) + "conf.cdi")
		if err != nil {
			fmt.Println("无法打开conf.cdi")
		}
		defer cdiFile.Close()
		//fmt.Println("Successfully Opened conf.cdi")
		cdiValue, _ := io.ReadAll(cdiFile)
		xml.Unmarshal([]byte(cdiValue), &t.EnvelopeN)
	}

}

func (t *Tiles) BuildBundleFilePath(level, row, col int) (bundlePath string) {
	baseRow := (row / _BUNDLX_MAXIDX) * _BUNDLX_MAXIDX
	baseCol := (col / _BUNDLX_MAXIDX) * _BUNDLX_MAXIDX

	zoomStr := strconv.FormatInt(int64(level), 10)
	if len(zoomStr) < 2 {
		zoomStr = "0" + zoomStr
	}
	r := fmt.Sprintf("%04x", baseRow)
	c := fmt.Sprintf("%04x", baseCol)
	bundlePath = "L" + zoomStr + "/" + "R" + r + "C" + c + _BUNDLE_EXT
	return
}

func (t *Tiles) GetTile(level, row, col int) []byte {
	bundlePath := t.BuildBundleFilePath(level, row, col)
	//f, _ := os.Open(bundlePath)
	//defer f.Close()
	f, success := t.openFile(bundlePath)
	if !success {
		return []byte{}
	}
	r := bytes.NewReader(f)
	index := _BUNDLX_MAXIDX*(row%_BUNDLX_MAXIDX) + (col % _BUNDLX_MAXIDX)
	r.Seek(int64(index*8+_COMPACT_CACHE_HEADER_LENGTH), 0)
	b1 := make([]byte, 8)
	r.Read(b1)
	b2 := make([]byte, 8)
	copy(b2, b1)
	offset := binary.LittleEndian.Uint64(append(b1[:5], 0, 0, 0))
	size := binary.LittleEndian.Uint32(append(b2[5:], 0))

	b3 := make([]byte, size)
	r.Seek(int64(offset), 0)
	r.Read(b3)
	return b3

}

func (t *Tiles) GetTilemap(level, row, col, width, height int) []byte {
	bundlePath := t.BuildBundleFilePath(level, row, col)
	//f, _ := os.Open(bundlePath)
	//defer f.Close()
	f, _ := t.openFile(bundlePath)
	r := bytes.NewReader(f)
	baseRow := (row / _BUNDLX_MAXIDX) * _BUNDLX_MAXIDX
	baseCol := (col / _BUNDLX_MAXIDX) * _BUNDLX_MAXIDX
	var adjusted bool = false
	var aheight = height
	if baseCol+_BUNDLX_MAXIDX-col < height {
		aheight = baseCol + _BUNDLX_MAXIDX
		adjusted = true
	}
	var awidth = width
	if baseRow+_BUNDLX_MAXIDX-row < width {
		awidth = baseRow + _BUNDLX_MAXIDX
		adjusted = true
	}
	data := make([]int, aheight*awidth)
	i := 0
	for w := 0; w < awidth; w++ {
		for h := 0; h < aheight; h++ {
			index := _BUNDLX_MAXIDX*((row+w)%_BUNDLX_MAXIDX) + ((col + h) % _BUNDLX_MAXIDX)
			r.Seek(int64(index*8+_COMPACT_CACHE_HEADER_LENGTH), 0)
			b1 := make([]byte, 8)
			r.Read(b1)
			//b2 := make([]byte, 8)
			//copy(b2,b1)
			//offset := binary.LittleEndian.Uint64(append(b1[:5],0,0,0))
			size := binary.LittleEndian.Uint32(append(b1[5:], 0))
			if size == 0 {
				data[i] = 0
			} else {
				data[i] = 1
			}
			i++
		}
	}
	tilemap := Tilemap{
		Adjusted: adjusted,
		Location: fmt.Sprintf("%d,%d,%d,%d", col, row, awidth, aheight),
		Data:     data,
	}
	result, _ := json.Marshal(tilemap)
	return result
}

func (t *Tiles) openFile(path string) ([]byte, bool) {
	if t.isTPKX {
		if path == "root.json" {
		} else {
			path = "tile/" + path
		}

		r, err := zip.OpenReader(t.RootPath)
		if err != nil {
			fmt.Printf("无法打开文件: %v", err)
		}
		defer r.Close()
		i := t.fileIndex[path]
		if i == 0 {
			return nil, false
		}
		rc, err := r.File[i].Open()
		if err != nil {
			fmt.Printf("读取%s失败", path)
			return nil, false
		}
		defer rc.Close()
		data, _ := io.ReadAll(rc)

		return data, true
	} else {
		data, err := os.ReadFile(t.RootPath + string(os.PathSeparator) + "_alllayers" + string(os.PathSeparator) + path)
		if err != nil {
			panic(err)
		}
		return data, true
	}
}
