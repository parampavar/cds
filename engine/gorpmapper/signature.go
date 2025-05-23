package gorpmapper

import (
	"bytes"
	"context"
	"crypto/sha1"
	"fmt"
	"text/template"

	"github.com/go-gorp/gorp"
	"github.com/ovh/symmecrypt"
	"github.com/rockbears/log"

	"github.com/ovh/cds/sdk"
)

// Constant for gorp mapping.
const (
	KeySignIdentifier = "db-sign"
)

type Signed interface {
	GetSignature() []byte
}

// Canonicaller returns a byte array that represent its data.
type Canonicaller interface {
	Canonical() CanonicalForms
}

// InsertAndSign a data in database, given data should implement canonicaller interface.
func (m *Mapper) InsertAndSign(ctx context.Context, db SqlExecutorWithTx, i Canonicaller) error {
	if err := m.Insert(db, i); err != nil {
		return err
	}
	return sdk.WithStack(m.dbSign(ctx, db, i))
}

// UpdateAndSign a data in database, given data should implement canonicaller interface.
func (m *Mapper) UpdateAndSign(ctx context.Context, db SqlExecutorWithTx, i Canonicaller) error {
	if err := m.Update(db, i); err != nil {
		return err
	}
	return sdk.WithStack(m.dbSign(ctx, db, i))
}

// UpdateColumnsAndSign a data in database, given data should implement canonicaller interface.
func (m *Mapper) UpdateColumnsAndSign(ctx context.Context, db SqlExecutorWithTx, i Canonicaller, colFilter gorp.ColumnFilter) error {
	if err := m.UpdateColumns(db, i, colFilter); err != nil {
		return err
	}
	return sdk.WithStack(m.dbSign(ctx, db, i))
}

// CheckSignature return true if a given signature is valid for given object.
func (m *Mapper) CheckSignature(i Canonicaller, sig []byte) (bool, error) {
	valid, _, err := m.CheckSignatureUncap(i, sig)
	return valid, err
}

func (m *Mapper) CheckSignatureUncap(i Canonicaller, sig []byte) (bool, int, error) {
	var canonicalForms = i.Canonical()
	var f *CanonicalForm
	for {
		f, canonicalForms = canonicalForms.Latest()
		if f == nil {
			return false, 0, nil
		}
		ok, keyIdx, err := m.checkSignature(i, f, sig)
		if err != nil {
			return ok, 0, err
		}
		if ok {
			return true, keyIdx, nil
		}
	}
}

func (m *Mapper) checkSignature(i Canonicaller, f *CanonicalForm, sig []byte) (bool, int, error) {
	tmpl, err := m.getCanonicalTemplate(f)
	if err != nil {
		return false, 0, err
	}

	var clearContent = new(bytes.Buffer)
	if err := tmpl.Execute(clearContent, i); err != nil {
		return false, 0, nil
	}

	var keyIdx int
	var decryptedSig []byte
	if cKey, ok := m.signatureKey.(symmecrypt.CompositeKey); ok {
		for idx, k := range cKey {
			decryptedSig, err = k.Decrypt(sig)
			if err == nil {
				keyIdx = idx
				break
			}
		}
	} else {
		decryptedSig, err = m.signatureKey.Decrypt(sig)
	}
	if err != nil {
		return false, 0, sdk.WrapError(err, "unable to decrypt content (%s)", string(sig))
	}

	clearContentStr := clearContent.String()
	res := clearContentStr == string(decryptedSig)

	return res, keyIdx, nil
}

func (m *Mapper) getCanonicalTemplate(f *CanonicalForm) (*template.Template, error) {
	sha := GetSigner(f)

	m.CanonicalFormTemplates.L.RLock()
	t, has := m.CanonicalFormTemplates.M[sha]
	m.CanonicalFormTemplates.L.RUnlock()

	if !has {
		return nil, sdk.WithStack(fmt.Errorf("no canonical function available"))
	}

	return t, nil
}

func GetSigner(f *CanonicalForm) string {
	h := sha1.New()
	_, _ = h.Write(f.Bytes())
	bs := h.Sum(nil)
	sha := fmt.Sprintf("%x", bs)
	return sha
}

func (m *Mapper) canonicalTemplate(data Canonicaller) (string, *template.Template, error) {
	f, _ := data.Canonical().Latest()
	if f == nil {
		return "", nil, sdk.WithStack(fmt.Errorf("no canonical function available for %T", data))
	}

	sha := GetSigner(f)

	m.CanonicalFormTemplates.L.RLock()
	t, has := m.CanonicalFormTemplates.M[sha]
	m.CanonicalFormTemplates.L.RUnlock()

	if !has {
		return "", nil, sdk.WithStack(fmt.Errorf("no canonical function available for %T", data))
	}

	return sha, t, nil
}

func (m *Mapper) sign(data Canonicaller) (string, []byte, error) {
	signer, tmpl, err := m.canonicalTemplate(data)
	if err != nil {
		return "", nil, err
	}

	if tmpl == nil {
		err := fmt.Errorf("unable to get canonical form template for %T", data)
		return "", nil, sdk.WrapError(err, "unable to sign data")
	}

	var clearContent = new(bytes.Buffer)
	if err := tmpl.Execute(clearContent, data); err != nil {
		return "", nil, sdk.WrapError(err, "unable to sign data")
	}

	btes, err := m.signatureKey.Encrypt(clearContent.Bytes())
	if err != nil {
		return "", nil, sdk.WithStack(fmt.Errorf("unable to encrypt content: %v", err))
	}

	return signer, btes, nil
}

func (m *Mapper) dbSign(ctx context.Context, db gorp.SqlExecutor, i Canonicaller) error {
	signer, signature, err := m.sign(i)
	if err != nil {
		return err
	}

	table, key, id, err := m.dbMappingPKey(i)
	if err != nil {
		return sdk.WrapError(err, "primary key field not found in table: %s", table)
	}

	query := fmt.Sprintf(`UPDATE "%s" SET sig = $2, signer = $3 WHERE %s = $1`, table, key)
	res, err := db.Exec(query, id, signature, signer)
	if err != nil {
		log.Error(ctx, "error executing query %s with parameters %s, %s: %v", query, table, key, err)
		return sdk.WithStack(err)
	}

	n, _ := res.RowsAffected()
	if n != 1 {
		return sdk.WithStack(fmt.Errorf("%d number of rows affected (table=%s, key=%s, id=%v)", n, table, key, id))
	}
	return nil
}
