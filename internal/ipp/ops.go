// SPDX-License-Identifier: AGPL-3.0-or-later

package ipp

// Operation ids (RFC 8011 section 5.4 and the CUPS extensions at 0x4000).
const (
	OpPrintJob             = 0x0002
	OpCancelJob            = 0x0008
	OpGetJobs              = 0x000A
	OpGetPrinterAttributes = 0x000B
	OpHoldJob              = 0x000C
	OpReleaseJob           = 0x000D
	OpRestartJob           = 0x000E
	OpPausePrinter         = 0x0010
	OpResumePrinter        = 0x0011
	OpCancelJobs           = 0x0038

	OpCUPSGetDefault       = 0x4001
	OpCUPSGetPrinters      = 0x4002
	OpCUPSAddModifyPrinter = 0x4003
	OpCUPSDeletePrinter    = 0x4004
	OpCUPSAcceptJobs       = 0x4008
	OpCUPSRejectJobs       = 0x4009
	OpCUPSSetDefault       = 0x400A
	OpCUPSGetDevices       = 0x400B
	OpCUPSGetPPDs          = 0x400C
	OpCUPSMoveJob          = 0x400D
)

// Status codes. Anything from 0x0100 up is an error.
const (
	StatusNotFound = 0x0406
)
