package ist

import "fmt"

// Event is one part of the IST as students are told to do it, in Spanish.
type Event struct {
	Name    string // "Push-ups"
	Measure string // how it is measured: "2 minutos", "en libras"
	HowTo   string
}

// Events lists the IST's parts in the order they are done; the tracking sheet and the intro
// dialog both print them.
var Events = []Event{
	{"Push-ups", "2 minutos", "Cuenta las repeticiones correctas en 2 minutos. Manos al ancho de los hombros y cuerpo recto; baja hasta que los brazos queden paralelos al piso y sube hasta estirarlos. Solo puedes descansar arriba."},
	{"Sit-ups", "2 minutos", "Cuenta las repeticiones correctas en 2 minutos. Rodillas dobladas, dedos entrelazados detrás de la cabeza y alguien sujetándote los pies; sube hasta que la espalda quede vertical y baja hasta que los omóplatos toquen el piso. Solo puedes descansar arriba."},
	{"Plancha", "el mayor tiempo posible", "Apoya los antebrazos y las puntas de los pies, con el cuerpo recto de los hombros a los tobillos. Aguanta el mayor tiempo posible; el tiempo para cuando la cadera o las rodillas tocan el piso."},
	{"Milla", "por tiempo", "Corre 1 milla lo más rápido que puedas: 4 vueltas a una pista de 400 m, o una ruta medida o una app de GPS. Anota el tiempo en minutos y segundos."},
	{"Peso", "en libras", "Pésate sin zapatos y con ropa ligera."},
	{"Cintura", "a la altura del ombligo", "Con una cinta métrica nivelada, mide alrededor del abdomen a la altura del ombligo, al final de una exhalación relajada y sin meter la barriga. Anota las pulgadas."},
}

// Safety is said before the test.
const Safety = "Calienta antes de empezar, toma agua y detente si sientes dolor o mareo."

// Clock formats seconds as m:ss, the way the mile and the plank are timed.
func Clock(seconds int) string { return fmt.Sprintf("%d:%02d", seconds/60, seconds%60) }
