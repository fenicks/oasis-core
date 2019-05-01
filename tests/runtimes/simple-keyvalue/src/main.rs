extern crate ekiden_keymanager_client;
extern crate ekiden_runtime;
extern crate failure;
extern crate io_context;
extern crate simple_keyvalue_api;

use std::sync::Arc;

use failure::Fallible;
use io_context::Context as IoContext;

use ekiden_keymanager_client::{ContractId, KeyManagerClient};
use ekiden_runtime::{
    common::crypto::hash::Hash,
    executor::Executor,
    rak::RAK,
    register_runtime_txn_methods, runtime_context,
    storage::{
        mkvs::{with_encryption_key, MKVS},
        StorageContext,
    },
    transaction::Context as TxnContext,
    Protocol, RpcDispatcher, TxnDispatcher,
};
use simple_keyvalue_api::{with_api, KeyValue};

// Include key manager enclave hash.
include!(concat!(env!("OUT_DIR"), "/km_enclave_hash.rs"));

struct Context {
    km_client: Arc<KeyManagerClient>,
}

/// Insert a key/value pair.
fn insert(args: &KeyValue, ctx: &mut TxnContext) -> Fallible<Option<String>> {
    ctx.emit_block_tag(b"kv_hello", b"insert");
    ctx.emit_txn_tag(b"kv_op", b"insert");
    ctx.emit_txn_tag(b"kv_key", args.key.as_bytes());

    let existing = StorageContext::with_current(|_cas, mkvs| {
        mkvs.insert(args.key.as_bytes(), args.value.as_bytes())
    });
    Ok(existing.map(|v| String::from_utf8(v)).transpose()?)
}

/// Retrieve a key/value pair.
fn get(args: &String, ctx: &mut TxnContext) -> Fallible<Option<String>> {
    ctx.emit_block_tag(b"kv_hello", b"get");
    ctx.emit_txn_tag(b"kv_op", b"get");
    ctx.emit_txn_tag(b"kv_key", args.as_bytes());

    let existing = StorageContext::with_current(|_cas, mkvs| mkvs.get(args.as_bytes()));
    Ok(existing.map(|v| String::from_utf8(v)).transpose()?)
}

/// Remove a key/value pair.
fn remove(args: &String, ctx: &mut TxnContext) -> Fallible<Option<String>> {
    ctx.emit_block_tag(b"kv_hello", b"remove");
    ctx.emit_txn_tag(b"kv_op", b"remove");
    ctx.emit_txn_tag(b"kv_key", args.as_bytes());

    let existing = StorageContext::with_current(|_cas, mkvs| mkvs.remove(args.as_bytes()));
    Ok(existing.map(|v| String::from_utf8(v)).transpose()?)
}

/// Helper for doing encrypted MKVS operations.
fn with_encryption<F, R>(ctx: &mut TxnContext, key: &[u8], f: F) -> Fallible<R>
where
    F: FnOnce(&mut MKVS) -> R,
{
    let rctx = runtime_context!(ctx, Context);

    // Derive contract ID based on key.
    let contract_id = ContractId::from(Hash::digest_bytes(key).as_ref());

    // Fetch encryption keys.
    let io_ctx = IoContext::create_child(&ctx.io_ctx);
    let result = rctx.km_client.get_or_create_keys(io_ctx, contract_id);
    let key = Executor::with_current(|executor| executor.block_on(result))?;

    // NOTE: This is only for example purposes, the correct way would be
    //       to also generate a (deterministic) nonce.

    StorageContext::with_current(|_cas, mkvs| {
        Ok(with_encryption_key(mkvs, key.state_key.as_ref(), f))
    })
}

/// (encrypted) Insert a key/value pair.
fn enc_insert(args: &KeyValue, ctx: &mut TxnContext) -> Fallible<Option<String>> {
    let existing = with_encryption(ctx, args.key.as_bytes(), |mkvs| {
        mkvs.insert(args.key.as_bytes(), args.value.as_bytes())
    })?;
    Ok(existing.map(|v| String::from_utf8(v)).transpose()?)
}

/// (encrypted) Retrieve a key/value pair.
fn enc_get(args: &String, ctx: &mut TxnContext) -> Fallible<Option<String>> {
    let existing = with_encryption(ctx, args.as_bytes(), |mkvs| mkvs.get(args.as_bytes()))?;
    Ok(existing.map(|v| String::from_utf8(v)).transpose()?)
}

/// (encrypted) Remove a key/value pair.
fn enc_remove(args: &String, ctx: &mut TxnContext) -> Fallible<Option<String>> {
    let existing = with_encryption(ctx, args.as_bytes(), |mkvs| mkvs.remove(args.as_bytes()))?;
    Ok(existing.map(|v| String::from_utf8(v)).transpose()?)
}

fn main() {
    // Initializer.
    let init = |protocol: &Arc<Protocol>,
                rak: &Arc<RAK>,
                _rpc: &mut RpcDispatcher,
                txn: &mut TxnDispatcher| {
        with_api! { register_runtime_txn_methods!(txn, api); }

        // Create the key manager client.
        let km_client = Arc::new(ekiden_keymanager_client::RemoteClient::new_runtime(
            #[cfg(target_env = "sgx")]
            Some(KM_ENCLAVE_HASH),
            #[cfg(not(target_env = "sgx"))]
            None,
            protocol.clone(),
            rak.clone(),
        ));

        txn.set_context_initializer(move |ctx: &mut TxnContext| {
            ctx.runtime = Box::new(Context {
                km_client: km_client.clone(),
            })
        });
    };

    // Start the runtime.
    ekiden_runtime::start_runtime(Some(Box::new(init)));
}