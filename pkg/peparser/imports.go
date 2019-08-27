package pe

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"log"
)

const (
	imageOrdinalFlag32   = uint32(0x80000000)
	imageOrdinalFlag64   = uint64(0x8000000000000000)
	maxRepeatedAddresses = uint32(16)
	maxAddressSpread     = uint32(0x8000000) // 64 MB
	addressMask32        = uint32(0x7fffffff)
	addressMask64        = uint64(0x7fffffffffffffff)
)

// ImageImportDescriptor describes the remainder of the import information.
// The import directory table contains address information that is used to resolve fixup references to the entry points within a DLL image.
// It consists of an array of import directory entries, one entry for each DLL to which the image refers.
// The last directory entry is empty (filled with null values), which indicates the end of the directory table.
type ImageImportDescriptor struct {
	OriginalFirstThunk uint32 // The RVA of the import lookup/name table (INT). This table contains a name or ordinal for each import. The INT is an array of IMAGE_THUNK_DATA structs.
	TimeDateStamp      uint32 // The stamp that is set to zero until the image is bound. After the image is bound, this field is set to the time/data stamp of the DLL.
	ForwarderChain     uint32 // The index of the first forwarder reference (-1 if no forwarders)
	Name               uint32 // The address of an ASCII string that contains the name of the DLL. This address is relative to the image base.
	FirstThunk         uint32 // The RVA of the import address table (IAT). The contents of this table are identical to the contents of the import lookup table until the image is bound.
}

// ImageThunkData32 corresponds to one imported function from the executable.
// The entries are an array of 32-bit numbers for PE32 or an array of 64-bit numbers for PE32+
// The ends of both arrays are indicated by an IMAGE_THUNK_DATA element with a value of zero.
// The IMAGE_THUNK_DATA union is a DWORD with these interpretations:
// DWORD Function;       // Memory address of the imported function
// DWORD Ordinal;        // Ordinal value of imported API
// DWORD AddressOfData;  // RVA to an IMAGE_IMPORT_BY_NAME with the imported API name
// DWORD ForwarderString;// RVA to a forwarder string
type ImageThunkData32 struct {
	AddressOfData uint32
}

// ImageThunkData64 is the PE32+ version of IMAGE_THUNK_DATA.
type ImageThunkData64 struct {
	AddressOfData uint64
}

// ImportFunction represents an imported function in the import table.
type ImportFunction struct {
	Name      string
	Hint      uint16
	Offset    uint32
	ByOrdinal bool
	Ordinal   uint32
	Address   uint32
	Bound     uint32
}

// Import represents an empty entry in the emport table.
type Import struct {
	Offset     uint32
	Name       string
	Functions  []*ImportFunction
	Descriptor ImageImportDescriptor
}

func (pe *File) parseImportDirectory(rva, size uint32) (err error) {

	for {
		importDesc := ImageImportDescriptor{}
		fileOffset := pe.getOffsetFromRva(rva)
		buf := bytes.NewReader(pe.data[fileOffset : fileOffset+size])
		err := binary.Read(buf, binary.LittleEndian, &importDesc)
		// If the RVA is invalid all would blow up. Some EXEs seem to be
		// specially nasty and have an invalid RVA.
		if err != nil {
			return err
		}

		// If the structure is all zeros, we reached the end of the list.
		if importDesc == (ImageImportDescriptor{}) {
			break
		}

		rva += uint32(binary.Size(importDesc))

		// If the array of thunks is somewhere earlier than the import
		// descriptor we can set a maximum length for the array. Otherwise
		// just set a maximum length of the size of the file
		maxLen := uint32(len(pe.data)) - fileOffset
		if rva > importDesc.OriginalFirstThunk ||
			rva > importDesc.FirstThunk {
			maxLen = Max(rva-importDesc.OriginalFirstThunk, rva-importDesc.FirstThunk)
		}

		var importedFunctions []*ImportFunction
		if pe.Is64 {
			importedFunctions, err = pe.parseImports64(&importDesc, maxLen)
		} else {
			importedFunctions, err = pe.parseImports32(&importDesc, maxLen)
		}
		if err != nil {
			return err
		}

		dllName := pe.getStringAtRVA(importDesc.Name)
		if !IsValidDosFilename(dllName) {
			dllName = "*invalid*"
			continue
		}

		pe.Imports = append(pe.Imports, Import{
			Offset:     fileOffset,
			Name:       string(dllName),
			Functions:  importedFunctions,
			Descriptor: importDesc,
		})

	}

	return nil
}

func (pe *File) getImportTable32(rva uint32, maxLen uint32) ([]*ImageThunkData32, error) {

	// Setup variables
	thunkTable := make(map[uint32]*ImageThunkData32)
	retVal := make([]*ImageThunkData32, 0)
	minAddressOfData := ^uint32(0)
	maxAddressOfData := uint32(0)
	repeatedAddress := uint32(0)
	var size uint32 = 4
	addressesOfData := make(map[uint32]bool)

	startRVA := rva
	for {
		if rva >= startRVA+maxLen {
			log.Println("Error parsing the import table. Entries go beyond bounds.")
			break
		}

		// if we see too many times the same entry we assume it could be
		// a table containing bogus data (with malicious intent or otherwise)
		if repeatedAddress >= maxAddressSpread {
			return []*ImageThunkData32{}, errors.New("bogus data found in imports")
		}

		// if the addresses point somewhere but the difference between the highest
		// and lowest address is larger than maxAddressSpread we assume a bogus
		// table as the addresses should be contained within a module
		if maxAddressOfData-minAddressOfData > maxAddressSpread {
			return []*ImageThunkData32{}, errors.New("data addresses too spread out")
		}

		// Read the image thunk data.
		thunk := ImageThunkData32{}
		offset := pe.getOffsetFromRva(rva)
		buf := bytes.NewReader(pe.data[offset : offset+size])

		err := binary.Read(buf, binary.LittleEndian, &thunk)
		if err != nil {
			msg := fmt.Sprintf("Error parsing the import table. Invalid data at RVA: 0x%x", rva)
			return []*ImageThunkData32{}, errors.New(msg)
		}

		if thunk == (ImageThunkData32{}) {
			break
		}

		// Check if the AddressOfData lies within the range of RVAs that it's
		// being scanned, abort if that is the case, as it is very unlikely
		// to be legitimate data.
		// Seen in PE with SHA256:
		// 5945bb6f0ac879ddf61b1c284f3b8d20c06b228e75ae4f571fa87f5b9512902c
		if thunk.AddressOfData >= startRVA && thunk.AddressOfData <= rva {
			log.Printf("Error parsing the import table. "+
				"AddressOfData overlaps with THUNK_DATA for THUNK at:\n  "+
				"RVA 0x%x", rva)
			break
		}

		// If the entry looks like could be an ordinal
		if thunk.AddressOfData&imageOrdinalFlag32 > 0 {
			// but its value is beyond 2^16, we will assume it's a
			// corrupted and ignore it altogether
			if thunk.AddressOfData&0x7fffffff > 0xffff {
				return []*ImageThunkData32{}, errors.New("beyond")
			}
		} else {
			// and if it looks like it should be an RVA
			// keep track of the RVAs seen and store them to study their
			// properties. When certain non-standard features are detected
			// the parsing will be aborted
			_, ok := addressesOfData[thunk.AddressOfData]
			if ok {
				repeatedAddress++
			} else {
				addressesOfData[thunk.AddressOfData] = true
			}

		}

		thunkTable[rva] = &thunk
		retVal = append(retVal, &thunk)
		rva += size
	}
	return retVal, nil
}

func (pe *File) getImportTable64(rva uint32, maxLen uint32) ([]*ImageThunkData64, error) {

	// Setup variables
	thunkTable := make(map[uint32]*ImageThunkData64)
	retVal := make([]*ImageThunkData64, 0)
	minAddressOfData := ^uint32(0)
	maxAddressOfData := uint32(0)
	repeatedAddress := uint32(0)
	var size uint32 = 8
	addressesOfData := make(map[uint64]bool)

	startRVA := rva
	for {
		if rva >= startRVA+maxLen {
			log.Println("Error parsing the import table. Entries go beyond bounds.")
			break
		}

		// if we see too many times the same entry we assume it could be
		// a table containing bogus data (with malicious intent or otherwise)
		if repeatedAddress >= maxAddressSpread {
			return []*ImageThunkData64{}, errors.New("bogus data found in imports")
		}

		// if the addresses point somewhere but the difference between the highest
		// and lowest address is larger than maxAddressSpread we assume a bogus
		// table as the addresses should be contained within a module
		if maxAddressOfData-minAddressOfData > maxAddressSpread {
			return []*ImageThunkData64{}, errors.New("data addresses too spread out")
		}

		// Read the image thunk data.
		thunk := ImageThunkData64{}
		offset := pe.getOffsetFromRva(rva)
		buf := bytes.NewReader(pe.data[offset : offset+size])

		err := binary.Read(buf, binary.LittleEndian, &thunk)
		if err != nil {
			msg := fmt.Sprintf("Error parsing the import table. Invalid data at RVA: 0x%x", rva)
			return []*ImageThunkData64{}, errors.New(msg)
		}

		if thunk == (ImageThunkData64{}) {
			break
		}

		// Check if the AddressOfData lies within the range of RVAs that it's
		// being scanned, abort if that is the case, as it is very unlikely
		// to be legitimate data.
		// Seen in PE with SHA256:
		// 5945bb6f0ac879ddf61b1c284f3b8d20c06b228e75ae4f571fa87f5b9512902c
		if thunk.AddressOfData >= uint64(startRVA) && thunk.AddressOfData <= uint64(rva) {
			log.Printf("Error parsing the import table. "+
				"AddressOfData overlaps with THUNK_DATA for THUNK at:\n  "+
				"RVA 0x%x", rva)
			break
		}

		// If the entry looks like could be an ordinal
		if thunk.AddressOfData&imageOrdinalFlag64 > 0 {
			// but its value is beyond 2^16, we will assume it's a
			// corrupted and ignore it altogether
			if thunk.AddressOfData&0x7fffffff > 0xffff {
				return []*ImageThunkData64{}, errors.New("beyond")
			}
		} else {
			// and if it looks like it should be an RVA
			// keep track of the RVAs seen and store them to study their
			// properties. When certain non-standard features are detected
			// the parsing will be aborted
			_, ok := addressesOfData[thunk.AddressOfData]
			if ok {
				repeatedAddress++
			} else {
				addressesOfData[thunk.AddressOfData] = true
			}

		}

		thunkTable[rva] = &thunk
		retVal = append(retVal, &thunk)
		rva += size
	}
	return retVal, nil
}

func (pe *File) parseImports32(importDesc *ImageImportDescriptor, maxLen uint32) ([]*ImportFunction, error) {

	// Import Lookup Table. Contains ordinals or pointers to strings.
	ilt, err := pe.getImportTable32(importDesc.OriginalFirstThunk, maxLen)
	if err != nil {
		return []*ImportFunction{}, err
	}

	// Import Address Table. May have identical content to ILT if PE file is
	// not bound. It will contain the address of the imported symbols once
	// the binary is loaded or if it is already bound.
	iat, err := pe.getImportTable32(importDesc.FirstThunk, maxLen)
	if err != nil {
		return []*ImportFunction{}, err
	}

	// Would crash if IAT or ILT had nil type
	if len(iat) == 0 && len(ilt) == 0 {
		return []*ImportFunction{}, errors.New("Damaged Import Table information. ILT and/or IAT appear to be broken")
	}

	var table []*ImageThunkData32
	if len(ilt) > 0 {
		table = ilt
	} else if len(iat) > 0 {
		table = iat
	} else {
		return []*ImportFunction{}, err
	}

	importOffset := uint32(0x4)
	importedFunctions := make([]*ImportFunction, 0)
	numInvalid := uint32(0)
	for idx := uint32(0); idx < uint32(len(table)); idx++ {
		imp := ImportFunction{}
		// imp.StructTable = table[idx]
		// imp.OrdinalOffset = table[idx].FileOffset

		if table[idx].AddressOfData > 0 {

			// If imported by ordinal, we will append the ordinal number
			if table[idx].AddressOfData&imageOrdinalFlag32 > 0 {
				imp.ByOrdinal = true
				imp.Ordinal = table[idx].AddressOfData & uint32(0xffff)
			} else {
				imp.ByOrdinal = false
				data, err := pe.getData(table[idx].AddressOfData&addressMask32, 2)
				if err != nil {
					return []*ImportFunction{}, err
				}
				imp.Hint = binary.LittleEndian.Uint16(data)
				imp.Name = pe.getStringAtRVA(table[idx].AddressOfData + 2)
				if !IsValidFunctionName(imp.Name) {
					imp.Name = "*invalid*"
				}
				imp.Offset = table[idx].AddressOfData
			}
		}

		imp.Address = importDesc.FirstThunk + pe.OptionalHeader.ImageBase + (idx * importOffset)

		if len(iat) > 0 && len(ilt) > 0 && ilt[idx].AddressOfData != iat[idx].AddressOfData {
			imp.Bound = iat[idx].AddressOfData
		}

		// The file with hashe:
		// SHA256: 3d22f8b001423cb460811ab4f4789f277b35838d45c62ec0454c877e7c82c7f5
		// has an invalid table built in a way that it's parseable but contains
		// invalid entries that lead pefile to take extremely long amounts of time to
		// parse. It also leads to extreme memory consumption. To prevent similar cases,
		// if invalid entries are found in the middle of a table the parsing will be aborted
		hasName := len(imp.Name) > 0
		if imp.Ordinal == 0 && !hasName {
			return []*ImportFunction{}, errors.New("Must have either an ordinal or a name in an import")
		}
		// Some PEs appear to interleave valid and invalid imports. Instead of
		// aborting the parsing altogether we will simply skip the invalid entries.
		// Although if we see 1000 invalid entries and no legit ones, we abort.
		if imp.Name == "*invalid*" {
			if numInvalid > 1000 && numInvalid == idx {
				return []*ImportFunction{}, errors.New("Too many invalid names, aborting parsing")
			}
			numInvalid++
			continue
		}

		if imp.Ordinal > 0 || hasName {
			importedFunctions = append(importedFunctions, &imp)
		}
	}

	return importedFunctions, nil
}

func (pe *File) parseImports64(importDesc *ImageImportDescriptor, maxLen uint32) ([]*ImportFunction, error) {

	// Import Lookup Table. Contains ordinals or pointers to strings.
	ilt, err := pe.getImportTable64(importDesc.OriginalFirstThunk, maxLen)
	if err != nil {
		return []*ImportFunction{}, err
	}

	// Import Address Table. May have identical content to ILT if PE file is
	// not bound. It will contain the address of the imported symbols once
	// the binary is loaded or if it is already bound.
	iat, err := pe.getImportTable64(importDesc.FirstThunk, maxLen)
	if err != nil {
		return []*ImportFunction{}, err
	}

	// Would crash if IAT or ILT had nil type
	if len(iat) == 0 && len(ilt) == 0 {
		return []*ImportFunction{}, errors.New("Damaged Import Table information. ILT and/or IAT appear to be broken")
	}

	var table []*ImageThunkData64
	if len(ilt) > 0 {
		table = ilt
	} else if len(iat) > 0 {
		table = iat
	} else {
		return []*ImportFunction{}, err
	}

	importOffset := uint32(0x8)
	importedFunctions := make([]*ImportFunction, 0)
	numInvalid := uint32(0)
	for idx := uint32(0); idx < uint32(len(table)); idx++ {
		imp := ImportFunction{}
		// imp.StructTable = table[idx]
		// imp.OrdinalOffset = table[idx].FileOffset

		if table[idx].AddressOfData > 0 {

			// If imported by ordinal, we will append the ordinal number
			if table[idx].AddressOfData&imageOrdinalFlag64 > 0 {
				imp.ByOrdinal = true
				imp.Ordinal = uint32(table[idx].AddressOfData) & uint32(0xffff)
			} else {
				imp.ByOrdinal = false
				data, err := pe.getData(uint32(table[idx].AddressOfData&addressMask64), 2)
				if err != nil {
					return []*ImportFunction{}, err
				}
				imp.Hint = binary.LittleEndian.Uint16(data)
				imp.Name = pe.getStringAtRVA(uint32(table[idx].AddressOfData + 2))
				if !IsValidFunctionName(imp.Name) {
					imp.Name = "*invalid*"
				}
				imp.Offset = uint32(table[idx].AddressOfData)
			}
		}

		imp.Address = importDesc.FirstThunk + pe.OptionalHeader.ImageBase + (idx * importOffset)

		if len(iat) > 0 && len(ilt) > 0 && ilt[idx].AddressOfData != iat[idx].AddressOfData {
			imp.Bound = uint32(iat[idx].AddressOfData)
		}

		// The file with hashe:
		// SHA256: 3d22f8b001423cb460811ab4f4789f277b35838d45c62ec0454c877e7c82c7f5
		// has an invalid table built in a way that it's parseable but contains
		// invalid entries that lead pefile to take extremely long amounts of time to
		// parse. It also leads to extreme memory consumption. To prevent similar cases,
		// if invalid entries are found in the middle of a table the parsing will be aborted
		hasName := len(imp.Name) > 0
		if imp.Ordinal == 0 && !hasName {
			return []*ImportFunction{}, errors.New("Must have either an ordinal or a name in an import")
		}
		// Some PEs appear to interleave valid and invalid imports. Instead of
		// aborting the parsing altogether we will simply skip the invalid entries.
		// Although if we see 1000 invalid entries and no legit ones, we abort.
		if imp.Name == "*invalid*" {
			if numInvalid > 1000 && numInvalid == idx {
				return []*ImportFunction{}, errors.New("Too many invalid names, aborting parsing")
			}
			numInvalid++
			continue
		}

		if imp.Ordinal > 0 || hasName {
			importedFunctions = append(importedFunctions, &imp)
		}
	}

	return importedFunctions, nil
}
