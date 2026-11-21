package provider

import "reflect"

// NewTransport is the client's transport builder, for TestTransportOrder.
var NewTransport = newTransport

// StartGateType is the start gate's type.
var StartGateType = reflect.TypeFor[startGate]()
