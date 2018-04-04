package server

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/asn1"
	"encoding/base64"
	"fmt"
	"io"
	"proj2_f5w9a_h6v9a_q7w9a_r8u8_w1c0b/serverpb"
	"strings"
	"time"

	"github.com/dgraph-io/badger"
	"github.com/pkg/errors"
)

func (s *Server) Get(ctx context.Context, in *serverpb.GetRequest) (*serverpb.GetResponse, error) {
	var f serverpb.Document
	documentId := strings.Split(in.GetAccessId(), ":")[0]
	parts := strings.Split(in.GetAccessId(), ":")
	if len(parts) < 2 {
		return nil, errors.Errorf("AccessId should have a :")
	}
	accessKey, err := base64.URLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, err
	}

	respRemote, err := s.GetRemoteFile(ctx, &serverpb.GetRemoteFileRequest{
		DocumentId: documentId,
		NumHops:    -1, // -1 tells GetRemoteFile to infer it.
	})
	if err != nil {
		return nil, err
	}
	if f, err = s.DecryptDocument(respRemote.Body, accessKey); err != nil {
		s.log.Println("cannot decrypt document", err)
		return nil, err
	}
	resp := &serverpb.GetResponse{
		Document: &f,
	}
	return resp, nil
}

func (s *Server) Add(ctx context.Context, in *serverpb.AddRequest) (*serverpb.AddResponse, error) {
	doc := in.GetDocument()
	if doc == nil {
		return nil, errors.New("missing Document")
	}
	encryptedDocument, key, err := s.EncryptDocument(*doc)
	if err != nil {
		return nil, err
	}

	hash := HashBytes(encryptedDocument)

	if err := s.db.Update(func(txn *badger.Txn) error {
		return txn.Set([]byte(fmt.Sprintf("/document/%s", hash)), encryptedDocument)
	}); err != nil {
		return nil, err
	}

	accessKey := base64.URLEncoding.EncodeToString(key)
	accessId := hash + ":" + accessKey
	resp := &serverpb.AddResponse{
		AccessId: accessId,
	}

	if err := s.addToRoutingTable(hash); err != nil {
		return nil, err
	}

	return resp, nil
}

func (s *Server) AddDirectory(ctx context.Context, in *serverpb.AddDirectoryRequest) (*serverpb.AddDirectoryResponse, error) {
	resp := &serverpb.AddDirectoryResponse{}
	return resp, nil
}

func (s *Server) GetPeers(ctx context.Context, in *serverpb.GetPeersRequest) (*serverpb.GetPeersResponse, error) {
	var peers []*serverpb.NodeMeta
	s.mu.Lock()
	defer s.mu.Unlock()

	for _, v := range s.mu.peerMeta {
		peers = append(peers, &v)
	}

	resp := &serverpb.GetPeersResponse{
		Peers: peers,
	}

	return resp, nil
}

func (s *Server) AddPeer(ctx context.Context, in *serverpb.AddPeerRequest) (*serverpb.AddPeerResponse, error) {
	err := s.BootstrapAddNode(ctx, in.GetAddr())
	if err != nil {
		return nil, err
	}
	resp := &serverpb.AddPeerResponse{}
	return resp, nil
}

func (s *Server) GetReference(ctx context.Context, in *serverpb.GetReferenceRequest) (*serverpb.GetReferenceResponse, error) {
	if in.GetReferenceId() == "" {
		return nil, errors.Errorf("missing reference_id")
	}

	resp, err := s.GetRemoteReference(ctx, &serverpb.GetRemoteReferenceRequest{
		ReferenceId: in.ReferenceId,
		NumHops:     -1,
	})
	if err != nil {
		return nil, err
	}

	var reference serverpb.Reference
	if err := reference.Unmarshal(resp.Reference); err != nil {
		return nil, err
	}

	signature, err := base64.URLEncoding.DecodeString(reference.Signature)
	if err != nil {
		return nil, err
	}
	var sig EcdsaSignature
	if _, err := asn1.Unmarshal(signature, &sig); err != nil {
		return nil, err
	}

	hash, err := Hash(reference.PublicKey)
	if err != nil {
		return nil, err
	}
	if hash != in.GetReferenceId() {
		return nil, errors.Errorf("public key doesn't match reference ID")
	}

	publicKey, err := UnmarshalPublic(reference.PublicKey)
	if err != nil {
		return nil, err
	}
	ref2 := reference
	ref2.Signature = ""
	bytes, err := ref2.Marshal()
	if err != nil {
		return nil, err
	}
	refHash := sha1.Sum(bytes)
	if !ecdsa.Verify(publicKey, refHash[:], sig.R, sig.S) {
		return nil, errors.Errorf("invalid signature received")
	}

	return &serverpb.GetReferenceResponse{
		Reference: &reference,
	}, nil
}

func (s *Server) AddReference(ctx context.Context, in *serverpb.AddReferenceRequest) (*serverpb.AddReferenceResponse, error) {
	privKey, err := LoadPrivate(in.GetPrivKey())
	if err != nil {
		return nil, err
	}
	pubKey, err := MarshalPublic(&privKey.PublicKey)
	if err != nil {
		return nil, err
	}
	// Create reference
	reference := &serverpb.Reference{
		Value:     in.GetRecord(),
		PublicKey: pubKey,
		Timestamp: time.Now().Unix(),
	}
	bytes, err := reference.Marshal()
	if err != nil {
		return nil, err
	}
	refHash := sha1.Sum(bytes)
	r, s1, err := Sign(refHash[:], *privKey)
	if err != nil {
		return nil, err
	}
	sig, err := asn1.Marshal(EcdsaSignature{R: r, S: s1})
	if err != nil {
		return nil, err
	}
	reference.Signature = base64.URLEncoding.EncodeToString(sig)

	// Add this reference locally
	referenceId, err := Hash(reference.PublicKey)
	if err != nil {
		return nil, err
	}
	b, err := reference.Marshal()
	if err != nil {
		return nil, err
	}

	if err := s.db.Update(func(txn *badger.Txn) error {
		return txn.Set([]byte(fmt.Sprintf("/reference/%s", referenceId)), b)
	}); err != nil {
		return nil, err
	}

	if err := s.addToRoutingTable(referenceId); err != nil {
		return nil, err
	}

	resp := &serverpb.AddReferenceResponse{
		ReferenceId: referenceId,
	}
	return resp, nil
}

func (s *Server) EncryptDocument(doc serverpb.Document) (encryptedData []byte, key []byte, err error) {
	// Create a new SHA256 handler
	shaHandler := sha256.New()

	marshalledData, err := doc.Marshal()
	if err != nil {
		return nil, nil, err
	}

	// Attempt to write the data to the SHA1 handler (is this right?)
	if _, err := shaHandler.Write(marshalledData); err != nil {
		// Well, error I guess
		return nil, nil, err
	}

	// Grab the SHA1 key
	docKey := shaHandler.Sum(nil)

	// Create a new AESBlockCipher
	aesBlock, err := aes.NewCipher(docKey)
	if err != nil {
		return nil, nil, err
	}

	ciphertext := make([]byte, aes.BlockSize+len(marshalledData))
	iv := ciphertext[:aes.BlockSize]
	if _, err := io.ReadFull(rand.Reader, iv); err != nil {
		return nil, nil, err
	}

	stream := cipher.NewCFBEncrypter(aesBlock, iv)
	stream.XORKeyStream(ciphertext[aes.BlockSize:], marshalledData)

	return ciphertext, docKey, nil
}

func (s *Server) DecryptDocument(documentData []byte, key []byte) (decryptedDocument serverpb.Document, err error) {
	aesBlock, err := aes.NewCipher(key)
	if err != nil {
		return serverpb.Document{}, err
	}

	if len(documentData) < aes.BlockSize {
		panic("ciphertext too short")
	}

	iv := documentData[:aes.BlockSize]
	documentData = documentData[aes.BlockSize:]

	stream := cipher.NewCFBDecrypter(aesBlock, iv)
	plainText := make([]byte, len(documentData))
	stream.XORKeyStream(plainText, documentData)

	err = decryptedDocument.Unmarshal(plainText)
	if err != nil {
		return serverpb.Document{}, err
	}

	return decryptedDocument, nil
}
