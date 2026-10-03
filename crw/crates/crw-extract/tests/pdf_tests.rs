//! Integration tests for the PDF → markdown adapter (`crw_extract::pdf`).
//! Gated on the `pdf` feature (default-on).

#![cfg(feature = "pdf")]

use crw_extract::pdf::{self, PdfError};
use lopdf::dictionary;

const SAMPLE: &[u8] = include_bytes!("fixtures/sample.pdf");

#[test]
fn text_pdf_extracts_markdown_and_plaintext() {
    let r = pdf::convert(SAMPLE, true, None, 0).expect("text PDF should convert");
    assert!(!r.is_scanned, "sample.pdf is text-based, not scanned");
    assert_eq!(r.page_count, 2, "fixture has two pages");
    assert!(
        r.markdown.contains("Hello fastCRW PDF parsing"),
        "markdown should contain the first-page heading; got: {}",
        r.markdown
    );
    assert!(
        r.markdown.contains("Second page content"),
        "markdown should include the second page"
    );
    assert!(
        r.plain_text.contains("Hello fastCRW PDF parsing"),
        "plaintext should carry the same text"
    );
}

#[test]
fn plaintext_skipped_when_not_requested() {
    let r = pdf::convert(SAMPLE, false, None, 0).expect("convert");
    assert!(!r.markdown.is_empty());
    assert!(
        r.plain_text.is_empty(),
        "plaintext pass is skipped when want_plaintext=false"
    );
}

#[test]
fn max_pages_caps_conversion() {
    let r = pdf::convert(SAMPLE, false, Some(1), 0).expect("convert with page cap");
    assert!(
        r.markdown.contains("Hello fastCRW PDF parsing"),
        "first page is present"
    );
    assert!(
        !r.markdown.contains("Second page content"),
        "second page should be excluded by max_pages=1"
    );
}

#[test]
fn oversized_page_limit_is_bounded_by_document_pages() {
    let r = pdf::convert(SAMPLE, false, Some(usize::MAX), 1024 * 1024)
        .expect("a page limit beyond the document should include every page");
    assert_eq!(r.page_count, 2);
    assert!(r.markdown.contains("Second page content"));
}

#[test]
fn metadata_title_survives_conversion() {
    let mut doc = lopdf::Document::load_mem(SAMPLE).expect("fixture");
    let info = doc.add_object(lopdf::dictionary! {
        "Title" => lopdf::Object::string_literal("CRW metadata regression"),
    });
    doc.trailer.set("Info", info);
    let mut bytes = Vec::new();
    doc.save_to(&mut bytes).expect("serialize fixture");
    let r = pdf::convert(&bytes, false, None, 1024 * 1024).expect("convert");
    assert_eq!(r.title.as_deref(), Some("CRW metadata regression"));
}

#[test]
fn encrypted_documents_remain_unsupported_including_empty_passwords() {
    for password in ["", "test-user-password"] {
        let mut doc = lopdf::Document::load_mem(SAMPLE).expect("fixture");
        let state = lopdf::EncryptionState::try_from(lopdf::EncryptionVersion::V2 {
            document: &doc,
            owner_password: "test-owner-password",
            user_password: password,
            key_length: 128,
            permissions: lopdf::Permissions::PRINTABLE,
        })
        .expect("encryption state");
        doc.encrypt(&state).expect("encrypt fixture");
        let mut bytes = Vec::new();
        doc.save_to(&mut bytes).expect("serialize fixture");
        for cap in [0, 1024 * 1024] {
            assert!(
                matches!(
                    pdf::convert(&bytes, false, None, cap),
                    Err(PdfError::Encrypted)
                ),
                "encrypted input must remain unsupported, including with cap={cap}"
            );
        }
    }
}

#[test]
fn compressed_object_stream_respects_preflight_budget() {
    use std::io::Write;

    let mut encoder = flate2::write::ZlibEncoder::new(Vec::new(), flate2::Compression::best());
    encoder
        .write_all(&[b' '; 64 * 1024])
        .expect("compress fixture");
    let mut doc = lopdf::Document::load_mem(SAMPLE).expect("fixture");
    doc.add_object(lopdf::Stream::new(
        lopdf::dictionary! {
            // lopdf's writer drops existing ObjStm containers. Serialize an
            // equal-width placeholder, then restore the type without changing
            // cross-reference offsets so the load path really sees the stream.
            "Type" => "RawStm",
            "N" => 0,
            "First" => 0,
            "Filter" => "FlateDecode",
        },
        encoder.finish().expect("finish compression"),
    ));
    let mut bytes = Vec::new();
    doc.save_to(&mut bytes).expect("serialize fixture");
    let marker = bytes
        .windows(b"/RawStm".len())
        .position(|window| window == b"/RawStm")
        .expect("serialized stream type");
    bytes[marker..marker + b"/ObjStm".len()].copy_from_slice(b"/ObjStm");
    let result = pdf::convert(&bytes, false, None, 1024);
    assert!(matches!(result, Err(PdfError::TooLarge)), "{result:?}");
}

#[test]
fn corrupt_bytes_error_without_panic() {
    // A header that looks like a PDF but is garbage must not panic (catch_unwind)
    // and must surface a parse error.
    let res = pdf::convert(b"%PDF-1.4\nthis is not a real pdf body", false, None, 0);
    assert!(res.is_err(), "corrupt PDF should error");
}

#[test]
fn non_pdf_bytes_rejected() {
    let res = pdf::convert(b"<html><body>not a pdf</body></html>", false, None, 0);
    assert!(matches!(
        res,
        Err(PdfError::NotAPdf) | Err(PdfError::Corrupt(_))
    ));
}

#[test]
fn error_codes_are_stable() {
    assert_eq!(PdfError::Encrypted.code(), "pdf_encrypted");
    assert_eq!(PdfError::NotAPdf.code(), "pdf_not_a_pdf");
    assert_eq!(PdfError::Corrupt("x".into()).code(), "pdf_parse_failed");
    assert_eq!(PdfError::Disabled.code(), "pdf_disabled");
}

/// RUSTSEC-2026-0187: lopdf <= 0.41 parsed nested arrays with unbounded
/// recursion, so a ~21 KB PDF whose Catalog holds a 10,000-deep array blew the
/// stack and aborted the process. A `SIGABRT` is not unwinding, so neither the
/// `catch_unwind` in `pdf::convert` nor this harness can trap it: on a
/// regression the whole test binary dies rather than reporting a failure, which
/// is the loudest signal available. Fixed in lopdf 0.42, which we reach through
/// pdf-inspector 1.17.
#[test]
fn deeply_nested_objects_do_not_abort() {
    let depth = 10_380;
    let nested: Vec<u8> = b"["
        .repeat(depth)
        .into_iter()
        .chain(b"]".repeat(depth))
        .collect();

    let objects: Vec<Vec<u8>> = vec![
        [
            b"1 0 obj\n<< /Type /Catalog /Pages 2 0 R /X ".as_slice(),
            &nested,
            b" >>\nendobj\n",
        ]
        .concat(),
        b"2 0 obj\n<< /Type /Pages /Kids [3 0 R] /Count 1 >>\nendobj\n".to_vec(),
        b"3 0 obj\n<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] >>\nendobj\n".to_vec(),
    ];

    let mut pdf = b"%PDF-1.4\n".to_vec();
    let mut offsets = Vec::new();
    for object in &objects {
        offsets.push(pdf.len());
        pdf.extend_from_slice(object);
    }
    let startxref = pdf.len();
    pdf.extend_from_slice(
        format!("xref\n0 {}\n0000000000 65535 f \n", objects.len() + 1).as_bytes(),
    );
    for offset in &offsets {
        pdf.extend_from_slice(format!("{offset:010} 00000 n \n").as_bytes());
    }
    pdf.extend_from_slice(
        format!(
            "trailer\n<< /Size {} /Root 1 0 R >>\nstartxref\n{startxref}\n%%EOF\n",
            objects.len() + 1
        )
        .as_bytes(),
    );

    // Reaching the assertion at all is the point: extracting nothing from a
    // page-less document is fine, aborting the process is not.
    let _ = pdf::convert(&pdf, false, None, 5 * 1024 * 1024);
}
