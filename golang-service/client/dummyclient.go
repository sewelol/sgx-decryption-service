package main

import (
	"bufio"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"fmt"
	"log"
	"os"
	"strings"

	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/pem"

	pb "github.com/sewelol/sgx-decryption-service/decryptionservice"
	"golang.org/x/net/context"
	"google.golang.org/grpc"
)

const (
	address     = "localhost:50051"
	defaultName = "world"
	rsaSpecTest = false
)

type leaf struct {
	Hash []byte
}

func main() {
	// Set up a connection to the server.
	conn, err := grpc.Dial(address, grpc.WithInsecure())
	if err != nil {
		log.Fatalf("did not connect: %v", err)
	}
	defer conn.Close()
	c := pb.NewDecryptionDeviceClient(conn)

	//  call GetRootTreeHash
	rth, err := c.GetRootTreeHash(context.Background(), &pb.RootTreeHashRequest{Nonce: []byte("aaaaaaaaa")})
	if err != nil {
		log.Fatalf("could not get rth: %v", err)
	}
	log.Printf("\nRTH: %s \nNonce: %s \nSignature: %s...\n\n", hex.EncodeToString(rth.Rth), hex.EncodeToString(rth.Nonce), hex.EncodeToString(rth.Sig[:31]))

	//  call GetPublicKey
	pk, err := c.GetPublicKey(context.Background(), &pb.PublicKeyRequest{Nonce: []byte("a long and random byte array")})
	if err != nil {
		log.Fatalf("could not get quote containing the public key: %v", err)
	}
	log.Printf("Quote: %s \n encryption key: %s \n verification key: %s\n\n", pk.Quote, pk.RSA_EncryptionKey, pk.RSA_VerificationKey)

	// import public keys
	encBlock, _ := pem.Decode(pk.RSA_EncryptionKey)
	verBlock, _ := pem.Decode(pk.RSA_VerificationKey)

	encPub, err := x509.ParsePKIXPublicKey(encBlock.Bytes)
	if err != nil {
		panic("failed to parse DER encoded public key: " + err.Error())
	}
	verPub, err := x509.ParsePKIXPublicKey(verBlock.Bytes)
	if err != nil {
		panic("failed to parse DER encoded public key: " + err.Error())
	}

	rsaEncPub, _ := encPub.(*rsa.PublicKey)
	rsaVerPub, _ := verPub.(*rsa.PublicKey)

	// test encryption OAEP padding
	rng := rand.Reader
	samplePlaintext := []byte("Decrypt RPC successfull (OAEP padding)") // If this string is printed in the response, all is well.
	label := []byte("record")
	sampleCiphertext, err := rsa.EncryptOAEP(sha256.New(), rng, rsaEncPub, samplePlaintext, label)
	if err != nil {
		log.Fatal(err)
	}

	// test encryption PKCS#1 v1.5 padding
	samplePKCS1v15PT := []byte("Decrypt RPC successfull (PKCS1v15 padding)")
	samplePKCS1v15CT, err := rsa.EncryptPKCS1v15(rng, rsaEncPub, samplePKCS1v15PT)
	if err != nil {
		log.Fatal(err)
	}

	log.Printf("\nEncryption test:\nCipher: RSA OAEP with sha256, \nplaintext(hex) = %s\nlabel(hex) = %s \nciphertext(hex) = %s",
		hex.EncodeToString(samplePlaintext),
		hex.EncodeToString(label),
		hex.EncodeToString(sampleCiphertext))

	if rsaSpecTest {
		// test decryption RPC using OAEP padding
		response, err := c.DecryptRecord(context.Background(), &pb.DecryptionRequest{Ciphertext: sampleCiphertext, ProofOfPresence: "{json proof...............}", ProofOfExtension: "{json proof...}"})
		if err != nil {
			log.Printf("could not decrypt record (OAEP padding): %v", err)
		} else {
			log.Printf("%s\n", response.Plaintext)
		}

		// test decryption RPC using PKCS#1 v1.5 padding
		response, err = c.DecryptRecord(context.Background(), &pb.DecryptionRequest{Ciphertext: samplePKCS1v15CT, ProofOfPresence: "{json proof...................}", ProofOfExtension: "{json proof...}"})
		if err != nil {
			log.Printf("could not decrypt record (PKCS#1 v1.5 padding): %v", err)
		} else {
			log.Printf("%s\n", response.Plaintext)
		}
	}

	// Verify RTH
	h := sha256.Sum256(append(rth.Rth, rth.Nonce...))

	err = rsa.VerifyPKCS1v15(rsaVerPub, crypto.SHA256, h[:], rth.Sig)
	if err != nil {
		log.Printf("failed to verify signed root tree hash: %v", err.Error())
	}
	log.Printf("Signed RTH verified (VerifyPKCS1v15): %s", hex.EncodeToString(rth.Rth))

	// Read encrypted records from file to a hash map
	ctDB := make(map[[32]byte][]byte)

	file, err := os.Open("test_set/records.csv")
	if err != nil {
		log.Fatal(err)
	}
	defer file.Close()

	// create a new scanner and read the file line by line
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.Split(scanner.Text(), ",")
		// Decode b64 ciphertext
		ct, err := base64.StdEncoding.DecodeString(line[1])
		// Calculate hash of ciphertext
		ctSum := sha256.Sum256(ct)
		if err != nil {
			log.Fatal(err)
		}
		ctDB[ctSum] = ct
	}
	// check for errors
	if err = scanner.Err(); err != nil {
		log.Fatal(err)
	}

	// Read proofs for records from file
	proofFile, err := os.Open("test_set/records_proofs.csv")
	if err != nil {
		log.Fatal(err)
	}
	defer proofFile.Close()

	presenceDB := make(map[string]string)
	extensionDB := make(map[string]string)

	// Scan proof_file
	// create a new scanner and read the proofs to proof maps
	scanner = bufio.NewScanner(proofFile)
	for scanner.Scan() {
		line := strings.Split(scanner.Text(), " ")

		presenceDB[line[0]] = line[1]
		extensionDB[line[0]] = line[2]

		ctSum := [32]byte{}
		ctSumSlice, err := hex.DecodeString(line[0])

		copy(ctSum[:], ctSumSlice)

		ct := ctDB[ctSum]
		pop := line[1]
		poe := line[2]

		r, err := c.DecryptRecord(context.Background(), &pb.DecryptionRequest{Ciphertext: ct, ProofOfPresence: pop, ProofOfExtension: poe})
		if err != nil {
			log.Printf("could not decrypt record: %v", err)
		} else {
			fmt.Printf("\rDecryptRecord(%s) = %d", hex.EncodeToString(ctSum[:]), r.Plaintext[0])

		}

	}
	// check for errors
	if err = scanner.Err(); err != nil {
		log.Fatal(err)
	}

	//  Remote call for DecryptRecord

}
