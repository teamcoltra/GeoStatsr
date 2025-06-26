package main

import (
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/paulmach/orb"
	"github.com/paulmach/orb/geojson"
	"github.com/paulmach/orb/planar"
)

// RegionFeatureProperties represents the properties of a country/region feature
type RegionFeatureProperties struct {
	ID           string   `json:"id"`
	ISO1A2       string   `json:"iso1A2"`
	ISO1A3       string   `json:"iso1A3"`
	ISO1N3       string   `json:"iso1N3"`
	M49          string   `json:"m49"`
	Wikidata     string   `json:"wikidata"`
	EmojiFlag    string   `json:"emojiFlag"`
	CCTLD        string   `json:"ccTLD"`
	NameEn       string   `json:"nameEn"`
	Aliases      []string `json:"aliases"`
	Country      string   `json:"country"`
	Groups       []string `json:"groups"`
	Members      []string `json:"members"`
	Level        string   `json:"level"`
	ISOStatus    string   `json:"isoStatus"`
	DriveSide    string   `json:"driveSide"`
	CallingCodes []string `json:"callingCodes"`
}

// RegionFeature represents a GeoJSON feature with region properties
type RegionFeature struct {
	Type       string                  `json:"type"`
	Properties RegionFeatureProperties `json:"properties"`
	Geometry   json.RawMessage         `json:"geometry"`
}

// RegionFeatureCollection represents a collection of region features
type RegionFeatureCollection struct {
	Type     string           `json:"type"`
	Features []*RegionFeature `json:"features"`
}

// CountryCoder provides country lookup functionality
type CountryCoder struct {
	features       []*geojson.Feature
	featuresByCode map[string]*geojson.Feature
	levels         []string
}

// CodingOptions for feature lookup
type CodingOptions struct {
	Level    string `json:"level"`
	MaxLevel string `json:"maxLevel"`
	WithProp string `json:"withProp"`
}

var (
	// Geographic levels, roughly from most to least granular
	defaultLevels = []string{
		"subterritory",
		"territory",
		"subcountryGroup",
		"country",
		"sharedLandform",
		"intermediateRegion",
		"subregion",
		"region",
		"subunion",
		"union",
		"unitedNations",
		"world",
	}

	// Filter regex for ID canonicalization - simplified for Go compatibility
	idFilterRegex = regexp.MustCompile(`\b(and|the|of|el|la|de)\b|[-_ .,'()&\[\]/]`)
)

// NewCountryCoder creates a new country coder from GeoJSON data
func NewCountryCoder(configDir string) *CountryCoder {
	var data []byte
	var err error

	// Try to read from external countries.json in config directory first
	if configDir != "" {
		externalCountriesPath := filepath.Join(configDir, "countries.json")
		if _, err := os.Stat(externalCountriesPath); err == nil {
			data, err = os.ReadFile(externalCountriesPath)
			if err == nil {
				debugLog("DEBUG: Loaded countries.json from config directory: %s", externalCountriesPath)
			} else {
				log.Printf("Warning: Failed to read external countries.json: %v", err)
			}
		}
	}

	// Fall back to embedded countries.json if external file not found or failed to read
	if data == nil {
		data, err = embeddedFS.ReadFile("countries.json")
		if err != nil {
			log.Fatalf("countries.json missing: %v", err)
		}
		debugLog("DEBUG: Loaded countries.json from embedded file")
	}

	var collection RegionFeatureCollection
	if err := json.Unmarshal(data, &collection); err != nil {
		log.Fatalf("bad GeoJSON: %v", err)
	}

	debugLog("DEBUG: Loaded %d features from countries.json", len(collection.Features))

	cc := &CountryCoder{
		features:       make([]*geojson.Feature, 0),
		featuresByCode: make(map[string]*geojson.Feature),
		levels:         defaultLevels,
	}

	// Convert to geojson.Feature format and build lookup maps
	for i, regionFeature := range collection.Features {
		feature := &geojson.Feature{
			Type: regionFeature.Type,
			Properties: map[string]interface{}{
				"id":           regionFeature.Properties.ID,
				"iso1A2":       regionFeature.Properties.ISO1A2,
				"iso1A3":       regionFeature.Properties.ISO1A3,
				"iso1N3":       regionFeature.Properties.ISO1N3,
				"m49":          regionFeature.Properties.M49,
				"wikidata":     regionFeature.Properties.Wikidata,
				"emojiFlag":    regionFeature.Properties.EmojiFlag,
				"ccTLD":        regionFeature.Properties.CCTLD,
				"nameEn":       regionFeature.Properties.NameEn,
				"aliases":      regionFeature.Properties.Aliases,
				"country":      regionFeature.Properties.Country,
				"groups":       regionFeature.Properties.Groups,
				"members":      regionFeature.Properties.Members,
				"level":        regionFeature.Properties.Level,
				"isoStatus":    regionFeature.Properties.ISOStatus,
				"driveSide":    regionFeature.Properties.DriveSide,
				"callingCodes": regionFeature.Properties.CallingCodes,
			},
		}

		// Parse geometry if it exists
		if len(regionFeature.Geometry) > 0 && string(regionFeature.Geometry) != "null" {
			debugLog("DEBUG: Feature %d (%s) has geometry data of length %d", i, regionFeature.Properties.NameEn, len(regionFeature.Geometry))

			// Parse geometry manually based on type
			var geomData map[string]interface{}
			if err := json.Unmarshal(regionFeature.Geometry, &geomData); err == nil {
				if geomType, ok := geomData["type"].(string); ok {
					if coords, ok := geomData["coordinates"]; ok {
						switch geomType {
						case "Polygon":
							if polygon := cc.coordsToPolygon(coords); polygon != nil {
								feature.Geometry = *polygon
								debugLog("DEBUG: Successfully parsed Polygon geometry for feature %d (%s)", i, regionFeature.Properties.NameEn)
							} else {
								debugLog("DEBUG: Failed to convert Polygon coordinates for feature %d (%s)", i, regionFeature.Properties.NameEn)
							}
						case "MultiPolygon":
							if multiPolygon := cc.coordsToMultiPolygon(coords); multiPolygon != nil {
								feature.Geometry = *multiPolygon
								debugLog("DEBUG: Successfully parsed MultiPolygon geometry for feature %d (%s)", i, regionFeature.Properties.NameEn)
							} else {
								debugLog("DEBUG: Failed to convert MultiPolygon coordinates for feature %d (%s)", i, regionFeature.Properties.NameEn)
							}
						case "Point":
							if coordsArray, ok := coords.([]interface{}); ok && len(coordsArray) >= 2 {
								if lng, ok1 := coordsArray[0].(float64); ok1 {
									if lat, ok2 := coordsArray[1].(float64); ok2 {
										feature.Geometry = orb.Point{lng, lat}
										debugLog("DEBUG: Successfully parsed Point geometry for feature %d (%s)", i, regionFeature.Properties.NameEn)
									}
								}
							}
						default:
							debugLog("DEBUG: Unsupported geometry type %s for feature %d (%s)", geomType, i, regionFeature.Properties.NameEn)
						}
					} else {
						debugLog("DEBUG: No coordinates found in geometry for feature %d (%s)", i, regionFeature.Properties.NameEn)
					}
				} else {
					debugLog("DEBUG: No type found in geometry for feature %d (%s)", i, regionFeature.Properties.NameEn)
				}
			} else {
				debugLog("DEBUG: Failed to parse geometry JSON for feature %d (%s): %v", i, regionFeature.Properties.NameEn, err)
			}
		} else {
			debugLog("DEBUG: Feature %d (%s) has no geometry data", i, regionFeature.Properties.NameEn)
		}

		cc.features = append(cc.features, feature)
		cc.cacheFeatureByIDs(feature)
	}

	debugLog("DEBUG: Processed %d features, %d have valid geometry", len(cc.features), func() int {
		count := 0
		for _, f := range cc.features {
			if f.Geometry != nil {
				count++
			}
		}
		return count
	}())

	return cc
}

// canonicalID normalizes an ID for lookup
func (cc *CountryCoder) canonicalID(id string) string {
	if id == "" {
		return ""
	}
	if strings.HasPrefix(id, ".") {
		// skip replace if it leads with a '.' (e.g. a ccTLD like '.de', '.la')
		return strings.ToUpper(id)
	}
	return strings.ToUpper(idFilterRegex.ReplaceAllString(id, ""))
}

// cacheFeatureByIDs caches features by their identifying strings for rapid lookup
func (cc *CountryCoder) cacheFeatureByIDs(feature *geojson.Feature) {
	props := []string{"id", "iso1A2", "iso1A3", "iso1N3", "m49", "wikidata", "emojiFlag", "ccTLD", "nameEn"}
	var ids []string

	for _, prop := range props {
		if val, ok := feature.Properties[prop].(string); ok && val != "" {
			ids = append(ids, val)
		}
	}

	// Add aliases if they exist
	if aliases, ok := feature.Properties["aliases"].([]interface{}); ok {
		for _, alias := range aliases {
			if aliasStr, ok := alias.(string); ok {
				ids = append(ids, aliasStr)
			}
		}
	}

	for _, id := range ids {
		cid := cc.canonicalID(id)
		if cid != "" {
			cc.featuresByCode[cid] = feature
		}
	}
}

// SmallestFeature returns the smallest feature of any kind containing the location
func (cc *CountryCoder) SmallestFeature(lat, lng float64) *geojson.Feature {
	debugLog("DEBUG: SmallestFeature called with lat=%f, lng=%f", lat, lng)
	pt := orb.Point{lng, lat}
	debugLog("DEBUG: SmallestFeature created point: %v", pt)
	debugLog("DEBUG: SmallestFeature checking %d features", len(cc.features))

	for i, feature := range cc.features {
		if feature.Geometry == nil {
			debugLog("DEBUG: SmallestFeature - feature %d has nil geometry, skipping", i)
			continue
		}

		// Log some info about this feature
		var featureName string
		if name, ok := feature.Properties["nameEn"].(string); ok {
			featureName = name
		} else if id, ok := feature.Properties["id"].(string); ok {
			featureName = id
		} else {
			featureName = "Unknown"
		}

		switch geom := feature.Geometry.(type) {
		case orb.Polygon:
			debugLog("DEBUG: SmallestFeature - checking feature %d (%s) - Polygon", i, featureName)
			if planar.PolygonContains(geom, pt) {
				debugLog("DEBUG: SmallestFeature - MATCH found in feature %d (%s) - Polygon", i, featureName)
				return feature
			}
		case orb.MultiPolygon:
			debugLog("DEBUG: SmallestFeature - checking feature %d (%s) - MultiPolygon", i, featureName)
			if planar.MultiPolygonContains(geom, pt) {
				debugLog("DEBUG: SmallestFeature - MATCH found in feature %d (%s) - MultiPolygon", i, featureName)
				return feature
			}
		default:
			debugLog("DEBUG: SmallestFeature - feature %d (%s) has unsupported geometry type: %T", i, featureName, geom)
		}
	}
	debugLog("DEBUG: SmallestFeature - no containing feature found")
	return nil
}

// coordsToPolygon converts coordinate interface to orb.Polygon
func (cc *CountryCoder) coordsToPolygon(coords interface{}) *orb.Polygon {
	if coordsArray, ok := coords.([]interface{}); ok {
		var rings []orb.Ring
		for _, ring := range coordsArray {
			if ringArray, ok := ring.([]interface{}); ok {
				var points []orb.Point
				for _, point := range ringArray {
					if pointArray, ok := point.([]interface{}); ok && len(pointArray) >= 2 {
						if lng, ok1 := pointArray[0].(float64); ok1 {
							if lat, ok2 := pointArray[1].(float64); ok2 {
								points = append(points, orb.Point{lng, lat})
							}
						}
					}
				}
				if len(points) > 0 {
					rings = append(rings, orb.Ring(points))
				}
			}
		}
		if len(rings) > 0 {
			polygon := orb.Polygon(rings)
			return &polygon
		}
	}
	return nil
}

// coordsToMultiPolygon converts coordinate interface to orb.MultiPolygon
func (cc *CountryCoder) coordsToMultiPolygon(coords interface{}) *orb.MultiPolygon {
	if coordsArray, ok := coords.([]interface{}); ok {
		var polygons []orb.Polygon
		for _, polygonCoords := range coordsArray {
			if polygon := cc.coordsToPolygon(polygonCoords); polygon != nil {
				polygons = append(polygons, *polygon)
			}
		}
		if len(polygons) > 0 {
			multiPolygon := orb.MultiPolygon(polygons)
			return &multiPolygon
		}
	}
	return nil
}

// FeatureForID returns the feature with an identifying property matching id
func (cc *CountryCoder) FeatureForID(id string) *geojson.Feature {
	cid := cc.canonicalID(id)
	return cc.featuresByCode[cid]
}

// Feature returns the feature matching the given arguments
func (cc *CountryCoder) Feature(query interface{}, opts *CodingOptions) *geojson.Feature {
	if opts == nil {
		opts = &CodingOptions{Level: "country"}
	}

	switch v := query.(type) {
	case string:
		return cc.FeatureForID(v)
	case []float64:
		if len(v) >= 2 {
			return cc.featureForLoc(v[1], v[0], opts) // lat, lng
		}
	case [2]float64:
		return cc.featureForLoc(v[1], v[0], opts) // lat, lng
	}
	return nil
}

// featureForLoc returns the feature containing the location for the given options
func (cc *CountryCoder) featureForLoc(lat, lng float64, opts *CodingOptions) *geojson.Feature {
	debugLog("DEBUG: featureForLoc called with lat=%f, lng=%f, opts=%+v", lat, lng, opts)

	targetLevel := opts.Level
	if targetLevel == "" {
		targetLevel = "country"
	}

	maxLevel := opts.MaxLevel
	if maxLevel == "" {
		maxLevel = "world"
	}

	withProp := opts.WithProp
	debugLog("DEBUG: featureForLoc - targetLevel=%s, maxLevel=%s, withProp=%s", targetLevel, maxLevel, withProp)

	// Fast path for country-level coding
	if targetLevel == "country" {
		debugLog("DEBUG: featureForLoc - taking fast path for country-level coding")
		feature := cc.countryFeature(lat, lng)
		if feature != nil {
			debugLog("DEBUG: featureForLoc - countryFeature returned feature with properties: %+v", feature.Properties)
			if withProp == "" || cc.hasProperty(feature, withProp) {
				debugLog("DEBUG: featureForLoc - fast path returning feature")
				return feature
			} else {
				debugLog("DEBUG: featureForLoc - feature doesn't have required property: %s", withProp)
			}
		} else {
			debugLog("DEBUG: featureForLoc - countryFeature returned nil")
		}
	}

	// General path - find smallest feature and walk up hierarchy
	debugLog("DEBUG: featureForLoc - taking general path")
	smallest := cc.SmallestFeature(lat, lng)
	if smallest == nil {
		debugLog("DEBUG: featureForLoc - SmallestFeature returned nil")
		return nil
	}
	debugLog("DEBUG: featureForLoc - SmallestFeature returned: %+v", smallest.Properties)

	targetLevelIndex := cc.levelIndex(targetLevel)
	maxLevelIndex := cc.levelIndex(maxLevel)

	if targetLevelIndex == -1 || maxLevelIndex == -1 || maxLevelIndex < targetLevelIndex {
		return nil
	}

	// Check if smallest feature matches criteria
	if cc.matchesLevel(smallest, targetLevel, maxLevel) {
		if withProp == "" || cc.hasProperty(smallest, withProp) {
			return smallest
		}
	}

	// Walk up the hierarchy through groups
	if groups, ok := smallest.Properties["groups"].([]interface{}); ok {
		for _, groupID := range groups {
			if groupIDStr, ok := groupID.(string); ok {
				groupFeature := cc.FeatureForID(groupIDStr)
				if groupFeature != nil && cc.matchesLevel(groupFeature, targetLevel, maxLevel) {
					if withProp == "" || cc.hasProperty(groupFeature, withProp) {
						return groupFeature
					}
				}
			}
		}
	}

	return nil
}

// countryFeature returns the country feature containing the location
func (cc *CountryCoder) countryFeature(lat, lng float64) *geojson.Feature {
	debugLog("DEBUG: countryFeature called with lat=%f, lng=%f", lat, lng)
	feature := cc.SmallestFeature(lat, lng)
	if feature == nil {
		debugLog("DEBUG: countryFeature - SmallestFeature returned nil")
		return nil
	}
	debugLog("DEBUG: countryFeature - SmallestFeature returned feature with properties: %+v", feature.Properties)

	// If feature has no country property but has geometry, it is itself a country
	if country, ok := feature.Properties["country"].(string); ok && country != "" {
		debugLog("DEBUG: countryFeature - found country property: %s", country)
		result := cc.FeatureForID(country)
		if result != nil {
			debugLog("DEBUG: countryFeature - FeatureForID(%s) returned feature", country)
		} else {
			debugLog("DEBUG: countryFeature - FeatureForID(%s) returned nil", country)
		}
		return result
	}

	if iso1A2, ok := feature.Properties["iso1A2"].(string); ok && iso1A2 != "" {
		debugLog("DEBUG: countryFeature - found iso1A2 property: %s", iso1A2)
		result := cc.FeatureForID(iso1A2)
		if result != nil {
			debugLog("DEBUG: countryFeature - FeatureForID(%s) returned feature", iso1A2)
		} else {
			debugLog("DEBUG: countryFeature - FeatureForID(%s) returned nil", iso1A2)
		}
		return result
	}

	debugLog("DEBUG: countryFeature - returning original feature")
	return feature
}

// hasProperty checks if feature has the given property
func (cc *CountryCoder) hasProperty(feature *geojson.Feature, prop string) bool {
	if val, exists := feature.Properties[prop]; exists {
		if str, ok := val.(string); ok {
			return str != ""
		}
		return val != nil
	}
	return false
}

// levelIndex returns the index of a level in the hierarchy
func (cc *CountryCoder) levelIndex(level string) int {
	for i, l := range cc.levels {
		if l == level {
			return i
		}
	}
	return -1
}

// matchesLevel checks if feature matches the target level or acceptable range
func (cc *CountryCoder) matchesLevel(feature *geojson.Feature, targetLevel, maxLevel string) bool {
	if level, ok := feature.Properties["level"].(string); ok {
		if level == targetLevel {
			return true
		}

		levelIndex := cc.levelIndex(level)
		targetLevelIndex := cc.levelIndex(targetLevel)
		maxLevelIndex := cc.levelIndex(maxLevel)

		// Return feature at next level up if no exact match
		return levelIndex > targetLevelIndex && levelIndex <= maxLevelIndex
	}
	return false
}

// ISO1A2Code returns the ISO 3166-1 alpha-2 code for the location
func (cc *CountryCoder) ISO1A2Code(lat, lng float64) string {
	debugLog("DEBUG: ISO1A2Code called with lat=%f, lng=%f", lat, lng)
	opts := &CodingOptions{WithProp: "iso1A2"}
	debugLog("DEBUG: ISO1A2Code calling featureForLoc with options: %+v", opts)
	feature := cc.featureForLoc(lat, lng, opts)
	if feature == nil {
		debugLog("DEBUG: ISO1A2Code - featureForLoc returned nil")
		return ""
	}
	debugLog("DEBUG: ISO1A2Code - featureForLoc returned feature with properties: %+v", feature.Properties)
	if code, ok := feature.Properties["iso1A2"].(string); ok {
		debugLog("DEBUG: ISO1A2Code returning: '%s'", code)
		return code
	}
	debugLog("DEBUG: ISO1A2Code - no iso1A2 property found")
	return ""
}

// NameEn returns the English name for the location
func (cc *CountryCoder) NameEn(lat, lng float64) string {
	feature := cc.featureForLoc(lat, lng, &CodingOptions{Level: "country"})
	if feature == nil {
		return ""
	}
	if name, ok := feature.Properties["nameEn"].(string); ok {
		return name
	}
	return ""
}

// NameEnByCode returns the English name for the given country code
func (cc *CountryCoder) NameEnByCode(code string) string {
	feature := cc.FeatureForID(code)
	if feature == nil {
		return strings.ToUpper(code) // fallback to uppercase code
	}
	if name, ok := feature.Properties["nameEn"].(string); ok {
		return name
	}
	return strings.ToUpper(code)
}

// CodeByLocation returns the country code for the location (falls back to old method if needed)
func (cc *CountryCoder) CodeByLocation(lat, lng float64) string {
	debugLog("DEBUG: CodeByLocation called with lat=%f, lng=%f", lat, lng)

	// Try the new method first
	code := cc.ISO1A2Code(lat, lng)
	debugLog("DEBUG: ISO1A2Code returned: '%s'", code)
	if code != "" {
		result := strings.ToLower(code)
		debugLog("DEBUG: CodeByLocation returning (new method): '%s'", result)
		return result
	}

	// Fallback to old method for compatibility
	debugLog("DEBUG: Falling back to old method, checking %d features", len(cc.features))
	pt := orb.Point{lng, lat}
	for i, feature := range cc.features {
		if feature.Geometry == nil {
			continue
		}

		switch geom := feature.Geometry.(type) {
		case orb.Polygon:
			if planar.PolygonContains(geom, pt) {
				result := cc.getCodeFromFeature(feature)
				debugLog("DEBUG: Found match in feature %d (Polygon), returning: '%s'", i, result)
				return result
			}
		case orb.MultiPolygon:
			if planar.MultiPolygonContains(geom, pt) {
				result := cc.getCodeFromFeature(feature)
				debugLog("DEBUG: Found match in feature %d (MultiPolygon), returning: '%s'", i, result)
				return result
			}
		}
	}
	debugLog("DEBUG: No matches found, returning '??'")
	return "??"
}

// getCodeFromFeature extracts country code from feature (for compatibility)
func (cc *CountryCoder) getCodeFromFeature(feature *geojson.Feature) string {
	if country, ok := feature.Properties["country"].(string); ok && country != "" {
		return strings.ToLower(country)
	}
	if nameEn, ok := feature.Properties["nameEn"].(string); ok && nameEn != "" {
		return strings.ToLower(nameEn)
	}
	if iso1A2, ok := feature.Properties["iso1A2"].(string); ok && iso1A2 != "" {
		return strings.ToLower(iso1A2)
	}
	return "??"
}

// geometryContainsPoint checks if geometry contains point
func (cc *CountryCoder) geometryContainsPoint(geom map[string]interface{}, pt orb.Point) bool {
	if geomType, ok := geom["type"].(string); ok {
		if coords, ok := geom["coordinates"]; ok {
			switch geomType {
			case "Polygon":
				if polygon := cc.coordsToPolygon(coords); polygon != nil {
					return planar.PolygonContains(*polygon, pt)
				}
			case "MultiPolygon":
				if multiPolygon := cc.coordsToMultiPolygon(coords); multiPolygon != nil {
					return planar.MultiPolygonContains(*multiPolygon, pt)
				}
			}
		}
	}
	return false
}
