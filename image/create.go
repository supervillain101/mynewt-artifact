/**
 * Licensed to the Apache Software Foundation (ASF) under one
 * or more contributor license agreements.  See the NOTICE file
 * distributed with this work for additional information
 * regarding copyright ownership.  The ASF licenses this file
 * to you under the Apache License, Version 2.0 (the
 * "License"); you may not use this file except in compliance
 * with the License.  You may obtain a copy of the License at
 *
 *  http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing,
 * software distributed under the License is distributed on an
 * "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
 * KIND, either express or implied.  See the License for the
 * specific language governing permissions and limitations
 * under the License.
 */

package image

import (
    "fmt"
	"bytes"
	"crypto"
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/asn1"
	"encoding/base64"
	"encoding/binary"
	"io/ioutil"
	"math/big"

	"github.com/apache/mynewt-artifact/errors"
	"github.com/apache/mynewt-artifact/sec"
	"golang.org/x/crypto/ed25519"
)

type ImageCreator struct {
	Body         []byte
	Version      ImageVersion
	SigKeys      []sec.PrivSignKey
	Sections     []Section
	HWKeyIndex   int
	Nonce        []byte
	PlainSecret  []byte
	CipherSecret []byte
	HeaderSize   int
	InitialHash  []byte
	Bootable     bool
	UseLegacyTLV bool
}

type ImageCreateOpts struct {
	SrcBinFilename    string
	SrcEncKeyFilename string
	SrcEncKeyIndex    int
	Version           ImageVersion
	SigKeys           []sec.PrivSignKey
	Sections          []Section
	LoaderHash        []byte
	HdrPad            int
	ImagePad          int
	UseLegacyTLV      bool
}

type ECDSASig struct {
	R *big.Int
	S *big.Int
}

func NewImageCreator() ImageCreator {
	return ImageCreator{
		HeaderSize: IMAGE_HEADER_SIZE,
		Bootable:   true,
	}
}

func sigTlvType(key sec.PrivSignKey) uint8 {
	key.AssertValid()

	if key.Rsa != nil {
		pubk := key.Rsa.Public().(*rsa.PublicKey)
		switch pubk.Size() {
		case 256:
			return IMAGE_TLV_RSA2048
		case 384:
			return IMAGE_TLV_RSA3072
		default:
			return 0
		}
	} else if key.Ec != nil {
		switch key.Ec.Curve.Params().Name {
		case "P-224":
			return IMAGE_TLV_ECDSA224
		case "P-256":
			return IMAGE_TLV_ECDSA256
		default:
			return 0
		}
	} else {
		return IMAGE_TLV_ED25519
	}
}

// GenerateHWKeyIndexTLV creates a hardware key index TLV.
func GenerateHWKeyIndexTLV(secretIndex uint32, useLegacyTLV bool) (ImageTlv, error) {
	var tlvType uint8
	id := make([]byte, 4)
	binary.LittleEndian.PutUint32(id, secretIndex)

	if useLegacyTLV {
		tlvType = IMAGE_TLV_SECRET_ID_LEGACY
	} else {
		tlvType = IMAGE_TLV_SECRET_ID
	}

	return ImageTlv{
		Header: ImageTlvHdr{
			Type: tlvType,
			Pad:  0,
			Len:  uint16(len(id)),
		},
		Data: id,
	}, nil
}

// GenerateNonceTLV creates a nonce TLV given a nonce.
func GenerateNonceTLV(nonce []byte, useLegacyTLV bool) (ImageTlv, error) {
	var tlvType uint8

	if useLegacyTLV {
		tlvType = IMAGE_TLV_AES_NONCE_LEGACY
	} else {
		tlvType = IMAGE_TLV_AES_NONCE
	}

	return ImageTlv{
		Header: ImageTlvHdr{
			Type: tlvType,
			Pad:  0,
			Len:  uint16(len(nonce)),
		},
		Data: nonce,
	}, nil
}

// GenerateEncTlv creates an encryption-secret TLV given a secret.
func GenerateEncTlv(cipherSecret []byte) (ImageTlv, error) {
	var encType uint8

	if len(cipherSecret) == 256 {
		encType = IMAGE_TLV_ENC_RSA
	} else if len(cipherSecret) == 113 {
		encType = IMAGE_TLV_ENC_EC256
	} else if len(cipherSecret) == 24 {
		encType = IMAGE_TLV_ENC_KEK
	} else {
		return ImageTlv{}, errors.Errorf("invalid enc TLV size: %d", len(cipherSecret))
	}

	return ImageTlv{
		Header: ImageTlvHdr{
			Type: encType,
			Pad:  0,
			Len:  uint16(len(cipherSecret)),
		},
		Data: cipherSecret,
	}, nil
}

// GenerateEncTlv creates an encryption-secret TLV given a secret.
func GenerateSectionTlv(section Section) (ImageTlv, error) {
	data := make([]byte, 8+len(section.Name))

	binary.LittleEndian.PutUint32(data[0:], uint32(section.Offset))
	binary.LittleEndian.PutUint32(data[4:], uint32(section.Size))
	copy(data[8:], section.Name)

	return ImageTlv{
		Header: ImageTlvHdr{
			Type: IMAGE_TLV_SECTION,
			Pad:  0,
			Len:  uint16(len(data)),
		},
		Data: data,
	}, nil
}

// GenerateSig signs an image using an rsa key.
func GenerateSigRsa(key sec.PrivSignKey, hash []byte) ([]byte, error) {
	opts := rsa.PSSOptions{
		SaltLength: rsa.PSSSaltLengthEqualsHash,
	}
	signature, err := rsa.SignPSS(
		rand.Reader, key.Rsa, crypto.SHA256, hash, &opts)
	if err != nil {
		return nil, errors.Wrapf(err, "failed to compute signature")
	}

	return signature, nil
}

// GenerateSig signs an image using an ec key.
func GenerateSigEc(key sec.PrivSignKey, hash []byte) ([]byte, error) {
	r, s, err := ecdsa.Sign(rand.Reader, key.Ec, hash)
	if err != nil {
		return nil, errors.Wrapf(err, "failed to compute signature")
	}

	ECDSA := ECDSASig{
		R: r,
		S: s,
	}

	signature, err := asn1.Marshal(ECDSA)
	if err != nil {
		return nil, errors.Wrapf(err, "failed to construct signature")
	}

	sigLen := key.SigLen()
	if len(signature) > int(sigLen) {
		return nil, errors.Errorf("signature truncated")
	}

	return signature, nil
}

// GenerateSig signs an image using an ed25519 key.
func GenerateSigEd25519(key sec.PrivSignKey, hash []byte) ([]byte, error) {
	sig := ed25519.Sign(*key.Ed25519, hash)

	if len(sig) != ed25519.SignatureSize {
		return nil, errors.Errorf(
			"ed25519 signature has wrong length: have=%d want=%d",
			len(sig), ed25519.SignatureSize)
	}

	return sig, nil
}

// GenerateSig signs an image.
func GenerateSig(key sec.PrivSignKey, hash []byte) (sec.Sig, error) {
	pub := key.PubKey()
	typ, err := pub.SigType()
	if err != nil {
		return sec.Sig{}, err
	}

	var data []byte

	switch typ {
	case sec.SIG_TYPE_RSA2048, sec.SIG_TYPE_RSA3072:
		data, err = GenerateSigRsa(key, hash)

	case sec.SIG_TYPE_ECDSA224, sec.SIG_TYPE_ECDSA256:
		data, err = GenerateSigEc(key, hash)

	case sec.SIG_TYPE_ED25519:
		data, err = GenerateSigEd25519(key, hash)

	default:
		err = errors.Errorf("unknown sig type: %v", typ)
	}

	if err != nil {
		return sec.Sig{}, err
	}

	keyHash, err := pub.Hash()
	if err != nil {
		return sec.Sig{}, err
	}

	return sec.Sig{
		Type:    typ,
		KeyHash: keyHash,
		Data:    data,
	}, nil
}

// BuildKeyHash produces a key-hash TLV given a public verification key.  Users
// do not normally need to call this.  Call BuildSigTlvs instead.
func BuildKeyHashTlv(keyBytes []byte) ImageTlv {
	data := sec.RawKeyHash(keyBytes)
	return ImageTlv{
		Header: ImageTlvHdr{
			Type: IMAGE_TLV_KEYHASH,
			Pad:  0,
			Len:  uint16(len(data)),
		},
		Data: data,
	}
}

// BuildSigTlvs signs an image and creates a pair of TLVs representing the
// signature.
func BuildSigTlvs(keys []sec.PrivSignKey, hash []byte) ([]ImageTlv, error) {
	var tlvs []ImageTlv

	for _, key := range keys {
		key.AssertValid()

		// Key hash TLV.
		pubKey, err := key.PubBytes()
		if err != nil {
			return nil, err
		}
		tlv := BuildKeyHashTlv(pubKey)
		tlvs = append(tlvs, tlv)

		// Signature TLV.
		sig, err := GenerateSig(key, hash)
		if err != nil {
			return nil, err
		}
		tlv = ImageTlv{
			Header: ImageTlvHdr{
				Type: sigTlvType(key),
				Len:  uint16(len(sig.Data)),
			},
			Data: sig.Data,
		}
		tlvs = append(tlvs, tlv)
	}

	return tlvs, nil
}

// GeneratePlainSecret randomly generates a 16-byte image-encrypting secret.
func GeneratePlainSecret() ([]byte, error) {
	plainSecret := make([]byte, 16)
	if _, err := rand.Read(plainSecret); err != nil {
		return nil, errors.Wrapf(err, "random generation error")
	}

	return plainSecret, nil
}

// GenerateImage produces an Image object from a set of image creation options.
func GenerateImage(opts ImageCreateOpts) (Image, error) {
	ic := NewImageCreator()

	srcBin, err := ioutil.ReadFile(opts.SrcBinFilename)
	if err != nil {
		return Image{}, errors.Wrapf(err, "Can't read app binary")
	}

	ic.Body = srcBin
	ic.Version = opts.Version
	ic.SigKeys = opts.SigKeys
	ic.HWKeyIndex = opts.SrcEncKeyIndex
	ic.Sections = opts.Sections
	ic.UseLegacyTLV = opts.UseLegacyTLV

	if opts.LoaderHash != nil {
		ic.InitialHash = opts.LoaderHash
		ic.Bootable = false
	} else {
		ic.Bootable = true
	}

	if opts.HdrPad > 0 {
		ic.HeaderSize = opts.HdrPad
	}

	if opts.ImagePad > 0 {
		tail_pad := opts.ImagePad - (len(ic.Body) % opts.ImagePad)
		ic.Body = append(ic.Body, bytes.Repeat([]byte{byte(0xff)}, tail_pad)...)
	}

	if ic.HWKeyIndex >= 0 {
		hash := sha256.Sum256(ic.Body)
		ic.Nonce = hash[:8]
	}

	if opts.SrcEncKeyFilename != "" {
		plainSecret, err := GeneratePlainSecret()
		if err != nil {
			return Image{}, err
		}

		pubKeBytes, err := ioutil.ReadFile(opts.SrcEncKeyFilename)
		if err != nil {
			return Image{}, errors.Wrapf(err, "error reading pubkey file")
		}

		if ic.HWKeyIndex < 0 {
			pubKe, err := sec.ParsePubEncKey(pubKeBytes)
			if err != nil {
				return Image{}, err
			}

			cipherSecret, err := pubKe.Encrypt(plainSecret)
			if err != nil {
				return Image{}, err
			}

			ic.CipherSecret = cipherSecret
			ic.PlainSecret = plainSecret
		} else {
			ic.PlainSecret, err = base64.StdEncoding.DecodeString(string(pubKeBytes))
			if err != nil {
				return Image{}, err
			}
		}
	}

	ri, err := ic.Create()
	if err != nil {
		return Image{}, err
	}

	return ri, nil
}

// calcHash calculates the sha256 for an image with the given components.
func calcHash(initialHash []byte, hdr ImageHdr, pad []byte,
	plainBody []byte, protTlvs []ImageTlv) ([]byte, error) {

    fmt.Printf("PHIL 2\n")

	hash := sha256.New()

	add := func(itf interface{}) error {
		b := &bytes.Buffer{}
		if err := binary.Write(b, binary.LittleEndian, itf); err != nil {
			return err
		}
		if err := binary.Write(hash, binary.LittleEndian, itf); err != nil {
			return errors.Wrapf(err, "failed to hash data")
		}

		return nil
	}

	if initialHash != nil {
		if err := add(initialHash); err != nil {
			return nil, err
		}
	}

	if err := add(hdr); err != nil {
		return nil, err
	}

	if err := add(pad); err != nil {
		return nil, err
	}

	if err := add(plainBody); err != nil {
		return nil, err
	}

	if len(protTlvs) > 0 {
		trailer := ImageTrailer{
			Magic:     IMAGE_PROT_TRAILER_MAGIC,
			TlvTotLen: hdr.ProtSz,
		}
		if err := add(trailer); err != nil {
			return nil, err
		}

		for _, tlv := range protTlvs {
			if err := add(tlv.Header); err != nil {
				return nil, err
			}
			if err := add(tlv.Data); err != nil {
				return nil, err
			}
		}
	}

	return hash.Sum(nil), nil
}

// calcProtSize calculates the size, in bytes, of a set of protected TLVs.
func calcProtSize(protTlvs []ImageTlv) uint16 {
	var size = uint16(0)
	for _, tlv := range protTlvs {
		size += IMAGE_TLV_SIZE
		size += tlv.Header.Len
	}
	if size > 0 {
		size += IMAGE_TRAILER_SIZE
	}
	return size
}

// Create produces an Image object.
func (ic *ImageCreator) Create() (Image, error) {
	img := Image{}

	// First the header
	img.Header = ImageHdr{
		Magic:  IMAGE_MAGIC,
		Pad1:   0,
		HdrSz:  IMAGE_HEADER_SIZE,
		ProtSz: 0,
		ImgSz:  uint32(len(ic.Body)),
		Flags:  0,
		Vers:   ic.Version,
		Pad3:   0,
	}

	if !ic.Bootable {
		img.Header.Flags |= IMAGE_F_NON_BOOTABLE
	}

	// Set encrypted image flag if image is to be treated as encrypted
	if ic.CipherSecret != nil && ic.HWKeyIndex < 0 {
		img.Header.Flags |= IMAGE_F_ENCRYPTED
	}

	if ic.HeaderSize != 0 {
		// Pad the header out to the given size.  There will just be zeros
		// between the header and the start of the image when it is padded.
		extra := ic.HeaderSize - IMAGE_HEADER_SIZE
		if extra < 0 {
			return img, errors.Errorf(
				"image header must be at least %d bytes", IMAGE_HEADER_SIZE)
		}

		img.Header.HdrSz = uint16(ic.HeaderSize)
		img.Pad = make([]byte, extra)
	}

	if ic.HWKeyIndex >= 0 {
		tlv, err := GenerateHWKeyIndexTLV(uint32(ic.HWKeyIndex),
			ic.UseLegacyTLV)
		if err != nil {
			return img, err
		}
		img.ProtTlvs = append(img.ProtTlvs, tlv)

		tlv, err = GenerateNonceTLV(ic.Nonce, ic.UseLegacyTLV)
		if err != nil {
			return img, err
		}
		img.ProtTlvs = append(img.ProtTlvs, tlv)
	}

	for s := range ic.Sections {
		tlv, err := GenerateSectionTlv(ic.Sections[s])
		if err != nil {
			return img, err
		}
		img.ProtTlvs = append(img.ProtTlvs, tlv)
	}

	img.Header.ProtSz = calcProtSize(img.ProtTlvs)

	// Followed by data.
	var hashBytes []byte
	var err error
	if ic.PlainSecret != nil {
		// For encrypted images, must calculate the hash with the plain
		// body and encrypt the payload afterwards
        fmt.Printf("PHILS MOD 1\n")
		img.Body = append(img.Body, ic.Body...)
		hashBytes, err = img.CalcHash(ic.InitialHash)
		if err != nil {
			return img, err
		}
		encBody, err := sec.EncryptAES(ic.Body, ic.PlainSecret, ic.Nonce)
		if err != nil {
			return img, err
		}
		img.Body = nil
		img.Body = append(img.Body, encBody...)
	} else {
		img.Body = append(img.Body, ic.Body...)
		hashBytes, err = img.CalcHash(ic.InitialHash)
		if err != nil {
			return img, err
		}
	}

	// Hash TLV.
	tlv := ImageTlv{
		Header: ImageTlvHdr{
			Type: IMAGE_TLV_SHA256,
			Pad:  0,
			Len:  uint16(len(hashBytes)),
		},
		Data: hashBytes,
	}
	img.Tlvs = append(img.Tlvs, tlv)

	tlvs, err := BuildSigTlvs(ic.SigKeys, hashBytes)
	if err != nil {
		return img, err
	}
	img.Tlvs = append(img.Tlvs, tlvs...)

	if ic.HWKeyIndex < 0 && ic.CipherSecret != nil {
		tlv, err := GenerateEncTlv(ic.CipherSecret)
		if err != nil {
			return img, err
		}
		img.Tlvs = append(img.Tlvs, tlv)
	}

	return img, nil
}
