package common

import (
	"encoding/xml"
	"bytes"
	"compress/gzip"
)

type LOD struct {
	XMLName    xml.Name `xml:"LODInfo" json:"-"`
	LevelID    int      `xml:"LevelID" json:"level"`
	Scale      float64  `xml:"Scale" json:"scale"`
	Resolution float64  `xml:"Resolution" json:"resolution"`
}

type SpatialReference struct {
	WKT         string  `json:"wkt,omitempty" xml:"WKT,omitempty"`
	WKID        int     `json:"wkid,omitempty" xml:"WKID"`
	LatestWKID  int     `json:"latestWkid,omitempty" xml:"LatestWKID"`
	XYTolerance float32 `json:"xyTolerance" xml:"XYTolerance"`
	ZTolerance  float32 `json:"zTolerance" xml:"ZTolerance"`
	MTolerance  float32 `json:"mTolerance" xml:"MTolerance"`
	XYUnits     float32 `json:"xyUnits" xml:"XYScale"`
	ZUnits      float32 `json:"zUnits" xml:"ZScale"`
	MUnits      float32 `json:"mUnits" xml:"MScale"`
	FalseX      float32 `json:"falseX" xml:"XOrigin"`
	FalseY      float32 `json:"falseY" xml:"YOrigin"`
	FalseZ      float32 `json:"falseZ" xml:"ZOrigin"`
	FalseM      float32 `json:"falseM" xml:"MOrigin"`
}

type Extent struct {
	Xmin             float64          `json:"xmin" xml:"XMin"`
	Ymin             float64          `json:"ymin" xml:"YMin"`
	Xmax             float64          `json:"xmax" xml:"XMax"`
	Ymax             float64          `json:"ymax" xml:"YMax"`
	SpatialReference SpatialReference `json:"spatialReference" xml:"SpatialReference"`
}

type Origin struct {
	X float64 `json:"x"`
	Y float64 `json:"y"`
}


type TileInfo struct {
	Rows               int              `json:"rows"`
	Cols               int              `json:"cols"`
	DPI                int16            `json:"dpi"`
	Format             string           `json:"format"`
	CompressionQuality float32          `json:"compressionQuality"`
	Origin             Origin           `json:"origin"`
	SpatialReference   SpatialReference `json:"spatialReference"`
	LODs               []LOD            `json:"lods"`
}

// 简短的gzip压缩函数
func Gzipdata(data []byte) ([]byte, error) {
	var b bytes.Buffer
	w := gzip.NewWriter(&b)
	if _, err := w.Write(data); err != nil {
		return nil, err
	}
	if err := w.Close(); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}
