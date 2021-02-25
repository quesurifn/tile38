package geojson

import "github.com/quesurifn/tile38/pkg/geojson/geohash"

// LineString is a geojson object with the type "LineString"
type LineString struct {
	Coordinates []Position
	BBox        *BBox
	bboxDefined bool
}

func fillLineString(coordinates []Position, bbox *BBox, err error) (LineString, error) {
	if err == nil {
		if len(coordinates) < 2 {
			err = errLineStringInvalidCoordinates
		}
	}
	bboxDefined := bbox != nil
	if !bboxDefined {
		cbbox := level2CalculatedBBox(coordinates, nil)
		bbox = &cbbox
	}
	return LineString{
		Coordinates: coordinates,
		BBox:        bbox,
		bboxDefined: bboxDefined,
	}, err
}

// CalculatedBBox is exterior bbox containing the object.
func (g LineString) CalculatedBBox() BBox {
	return level2CalculatedBBox(g.Coordinates, g.BBox)
}

// CalculatedPoint is a point representation of the object.
func (g LineString) CalculatedPoint() Position {
	return g.CalculatedBBox().center()
}

// Geohash converts the object to a geohash value.
func (g LineString) Geohash(precision int) (string, error) {
	p := g.CalculatedPoint()
	return geohash.Encode(p.Y, p.X, precision)
}

// PositionCount return the number of coordinates.
func (g LineString) PositionCount() int {
	return level2PositionCount(g.Coordinates, g.BBox)
}

// Weight returns the in-memory size of the object.
func (g LineString) Weight() int {
	return level2Weight(g.Coordinates, g.BBox)
}

func (g LineString) appendJSON(json []byte) []byte {
	return appendLevel2JSON(json, "LineString", g.Coordinates, g.BBox, g.bboxDefined)
}

// MarshalJSON allows the object to be encoded in json.Marshal calls.
func (g LineString) MarshalJSON() ([]byte, error) {
	return g.appendJSON(nil), nil
}

// JSON is the json representation of the object. This might not be exactly the same as the original.
func (g LineString) JSON() string {
	return string(g.appendJSON(nil))
}

// String returns a string representation of the object. This might be JSON or something else.
func (g LineString) String() string {
	return g.JSON()
}

func (g LineString) bboxPtr() *BBox {
	return g.BBox
}

func (g LineString) hasPositions() bool {
	return g.bboxDefined || len(g.Coordinates) > 0
}

// WithinBBox detects if the object is fully contained inside a bbox.
func (g LineString) WithinBBox(bbox BBox) bool {
	if g.bboxDefined {
		return rectBBox(g.CalculatedBBox()).InsideRect(rectBBox(bbox))
	}
	return polyPositions(g.Coordinates).InsideRect(rectBBox(bbox))
}

// IntersectsBBox detects if the object intersects a bbox.
func (g LineString) IntersectsBBox(bbox BBox) bool {
	if g.bboxDefined {
		return rectBBox(g.CalculatedBBox()).IntersectsRect(rectBBox(bbox))
	}
	return polyPositions(g.Coordinates).IntersectsRect(rectBBox(bbox))
}

// Within detects if the object is fully contained inside another object.
func (g LineString) Within(o Object) bool {
	return withinObjectShared(g, o,
		func(v Polygon) bool {
			return polyPositions(g.Coordinates).Inside(polyExteriorHoles(v.Coordinates))
		},
	)
}

// WithinCircle detects if the object is fully contained inside a circle.
func (g LineString) WithinCircle(center Position, meters float64) bool {
	if len(g.Coordinates) == 0 {
		return false
	}
	for _, position := range g.Coordinates {
		if center.DistanceTo(position) >= meters {
			return false
		}
	}
	return true
}

// Intersects detects if the object intersects another object.
func (g LineString) Intersects(o Object) bool {
	return intersectsObjectShared(g, o,
		func(v Polygon) bool {
			return polyPositions(g.Coordinates).LineStringIntersects(polyExteriorHoles(v.Coordinates))
		},
	)
}

// IntersectsCircle detects if the object intersects a circle.
func (g LineString) IntersectsCircle(center Position, meters float64) bool {
	for i := 0; i < len(g.Coordinates) - 1 ; i++ {
		if SegmentIntersectsCircle(g.Coordinates[i], g.Coordinates[i + 1], center, meters) {
			return true
		}
	}
	return false
}

// Nearby detects if the object is nearby a position.
func (g LineString) Nearby(center Position, meters float64) bool {
	return nearbyObjectShared(g, center.X, center.Y, meters)
}

// IsBBoxDefined returns true if the object has a defined bbox.
func (g LineString) IsBBoxDefined() bool {
	return g.bboxDefined
}

// IsGeometry return true if the object is a geojson geometry object. false if it something else.
func (g LineString) IsGeometry() bool {
	return true
}
