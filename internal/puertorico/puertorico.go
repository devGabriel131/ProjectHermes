// Package puertorico holds the Puerto Rico civil time zone.
package puertorico

import "time"

// Zone uses UTC-4 without modern daylight saving time.
var Zone = time.FixedZone("America/Puerto_Rico", -4*60*60)
