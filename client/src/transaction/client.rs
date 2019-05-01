//! Transaction client.
use std::time::Duration;

use failure::{Error, Fallible};
use futures::{future, prelude::*};
use grpcio::Channel;
use rustracing::{sampler::AllSampler, tag};
use rustracing_jaeger::{span::Span, Tracer};
use serde::{de::DeserializeOwned, Serialize};
use serde_cbor;

use ekiden_runtime::{
    common::{crypto::hash::Hash, runtime::RuntimeId},
    transaction::types::{TxnBatch, TxnCall, TxnOutput},
};

use super::{
    api,
    block_watcher::BlockWatcher,
    snapshot::{BlockSnapshot, TransactionSnapshot},
};
use crate::BoxFuture;

/// Special round number always referring to the latest round.
pub const ROUND_LATEST: u64 = u64::max_value();
/// Tag used for storing the Ekiden block hash.
pub const TAG_BLOCK_HASH: &'static [u8] = b"hblk";

/// Transaction client error.
#[derive(Debug, Fail)]
pub enum TxnClientError {
    #[fail(display = "node call failed: {}", 0)]
    CallFailed(String),
    #[fail(display = "block watcher closed")]
    WatcherClosed,
    #[fail(display = "transaction failed: {}", 0)]
    TxnFailed(String),
}

/// Interface for the node's client interface.
pub struct TxnClient {
    /// The underlying client gRPC interface.
    client: api::client::RuntimeClient,
    /// The underlying storage gRPC interface.
    storage_client: api::storage::StorageClient,
    /// Runtime identifier.
    runtime_id: RuntimeId,
    /// RPC timeout.
    timeout: Option<Duration>,
    /// Block watcher for `get_latest_block` call.
    block_watcher: BlockWatcher,
}

impl TxnClient {
    /// Create a new transaction client.
    pub fn new(channel: Channel, runtime_id: RuntimeId, timeout: Option<Duration>) -> Self {
        Self {
            client: api::client::RuntimeClient::new(channel.clone()),
            storage_client: api::storage::StorageClient::new(channel),
            runtime_id: runtime_id.clone(),
            timeout: timeout,
            block_watcher: BlockWatcher::new(),
        }
    }

    /// Call a remote method.
    pub fn call<C, O>(&self, method: &'static str, args: C) -> BoxFuture<O>
    where
        C: Serialize,
        O: DeserializeOwned + Send + 'static,
    {
        let call = TxnCall {
            method: method.to_owned(),
            args: match serde_cbor::to_value(args) {
                Ok(args) => args,
                Err(error) => return Box::new(future::err(error.into())),
            },
        };

        Box::new(
            self.submit_tx_raw(&call)
                .and_then(|out| parse_call_output(out)),
        )
    }

    /// Dispatch a raw call to the node.
    pub fn submit_tx_raw<C>(&self, call: C) -> BoxFuture<Vec<u8>>
    where
        C: Serialize,
    {
        let (span, options) = self.prepare_options("TxnClient::submit_tx_raw");

        let mut request = api::client::SubmitTxRequest::new();
        request.set_runtime_id(self.runtime_id.as_ref().to_vec());
        match serde_cbor::to_vec(&call) {
            Ok(data) => request.set_data(data),
            Err(error) => return Box::new(future::err(error.into())),
        }

        match self.client.submit_tx_async_opt(&request, options) {
            Ok(resp) => Box::new(
                resp.map(|r| {
                    drop(span);
                    r.result
                })
                .map_err(|error| TxnClientError::CallFailed(format!("{}", error)).into()),
            ),
            Err(error) => Box::new(future::err(
                TxnClientError::CallFailed(format!("{}", error)).into(),
            )),
        }
    }

    /// Wait for the node to finish syncing.
    pub fn wait_sync(&self) -> BoxFuture<()> {
        let (span, options) = self.prepare_options("TxnClient::wait_sync");
        let request = api::client::WaitSyncRequest::new();

        let result: BoxFuture<()> = match self.client.wait_sync_async_opt(&request, options) {
            Ok(resp) => Box::new(
                resp.map(|_| ())
                    .map_err(|error| TxnClientError::CallFailed(format!("{}", error)).into()),
            ),
            Err(error) => Box::new(future::err(
                TxnClientError::CallFailed(format!("{}", error)).into(),
            )),
        };
        drop(span);
        result
    }

    /// Check if the node is finished syncing.
    pub fn is_synced(&self) -> BoxFuture<bool> {
        let (span, options) = self.prepare_options("TxnClient::is_synced");
        let request = api::client::IsSyncedRequest::new();

        let result: BoxFuture<bool> = match self.client.is_synced_async_opt(&request, options) {
            Ok(resp) => Box::new(
                resp.map(|r| r.synced)
                    .map_err(|error| TxnClientError::CallFailed(format!("{}", error)).into()),
            ),
            Err(error) => Box::new(future::err(
                TxnClientError::CallFailed(format!("{}", error)).into(),
            )),
        };
        drop(span);
        result
    }

    /// Retrieve the latest block snapshot.
    pub fn get_latest_block(&self) -> BoxFuture<BlockSnapshot> {
        let block_watcher = self.block_watcher.clone();
        let runtime_id = self.runtime_id.clone();
        let client = self.client.clone();
        let storage_client = self.storage_client.clone();

        Box::new(future::lazy(move || -> BoxFuture<BlockSnapshot> {
            // Spawn block watcher if not running yet.
            if block_watcher.start_spawn() {
                let mut request = api::client::WatchBlocksRequest::new();
                request.set_runtime_id(runtime_id.as_ref().to_vec());

                let block_watcher = block_watcher.clone();
                match client.watch_blocks(&request) {
                    Ok(blocks) => {
                        block_watcher.spawn(
                            blocks
                                .map_err(|err| -> Error { err.into() })
                                .and_then(move |rsp| {
                                    Ok(BlockSnapshot::new(
                                        storage_client.clone(),
                                        serde_cbor::from_slice(&rsp.block)?,
                                        Hash::from(rsp.block_hash),
                                    ))
                                }),
                        );
                    }
                    Err(error) => {
                        // Failed to start watching blocks, retry on next attempt.
                        block_watcher.cancel_spawn();
                        return Box::new(future::err(
                            TxnClientError::CallFailed(format!("{}", error)).into(),
                        ));
                    }
                }
            }

            Box::new(block_watcher.get_latest_block().map_err(|err| err.into()))
        }))
    }

    // Retrieve block snapshot at specified round.
    pub fn get_block(&self, round: u64) -> BoxFuture<BlockSnapshot> {
        let (span, options) = self.prepare_options("TxnClient::get_block");
        let mut request = api::client::GetBlockRequest::new();
        request.set_runtime_id(self.runtime_id.as_ref().to_vec());
        request.set_round(round);

        let result: BoxFuture<BlockSnapshot> = match self
            .client
            .get_block_async_opt(&request, options)
        {
            Ok(resp) => {
                let storage_client = self.storage_client.clone();
                Box::new(
                    resp.map_err(|error| TxnClientError::CallFailed(format!("{}", error)).into())
                        .and_then(move |rsp| {
                            Ok(BlockSnapshot::new(
                                storage_client,
                                serde_cbor::from_slice(&rsp.block)?,
                                Hash::from(rsp.block_hash),
                            ))
                        }),
                )
            }
            Err(error) => Box::new(future::err(
                TxnClientError::CallFailed(format!("{}", error)).into(),
            )),
        };
        drop(span);
        result
    }

    // Retrieve transaction at specified block round and index.
    pub fn get_txn(&self, round: u64, index: u32) -> BoxFuture<TransactionSnapshot> {
        let (span, options) = self.prepare_options("TxnClient::get_txn");
        let mut request = api::client::GetTxnRequest::new();
        request.set_runtime_id(self.runtime_id.as_ref().to_vec());
        request.set_round(round);
        request.set_index(index);

        let result: BoxFuture<TransactionSnapshot> = match self
            .client
            .get_txn_async_opt(&request, options)
        {
            Ok(resp) => {
                let storage_client = self.storage_client.clone();
                Box::new(
                    resp.map_err(|error| TxnClientError::CallFailed(format!("{}", error)).into())
                        .and_then(move |mut rsp| {
                            TransactionSnapshot::new(
                                storage_client,
                                serde_cbor::from_slice(&rsp.block)?,
                                Hash::from(rsp.take_block_hash()),
                                index,
                                rsp.take_input(),
                                rsp.take_output(),
                            )
                        }),
                )
            }
            Err(error) => Box::new(future::err(
                TxnClientError::CallFailed(format!("{}", error)).into(),
            )),
        };
        drop(span);
        result
    }

    // Retrieve transaction at specified block hash and index.
    pub fn get_txn_by_block_hash(
        &self,
        block_hash: Hash,
        index: u32,
    ) -> BoxFuture<TransactionSnapshot> {
        let (span, options) = self.prepare_options("TxnClient::get_txn");
        let mut request = api::client::GetTxnByBlockHashRequest::new();
        request.set_runtime_id(self.runtime_id.as_ref().to_vec());
        request.set_block_hash(block_hash.as_ref().to_vec());
        request.set_index(index);

        let result: BoxFuture<TransactionSnapshot> = match self
            .client
            .get_txn_by_block_hash_async_opt(&request, options)
        {
            Ok(resp) => {
                let storage_client = self.storage_client.clone();
                Box::new(
                    resp.map_err(|error| TxnClientError::CallFailed(format!("{}", error)).into())
                        .and_then(move |mut rsp| {
                            TransactionSnapshot::new(
                                storage_client,
                                serde_cbor::from_slice(&rsp.block)?,
                                Hash::from(rsp.take_block_hash()),
                                index,
                                rsp.take_input(),
                                rsp.take_output(),
                            )
                        }),
                )
            }
            Err(error) => Box::new(future::err(
                TxnClientError::CallFailed(format!("{}", error)).into(),
            )),
        };
        drop(span);
        result
    }

    // Retrieve transactions at specific root.
    pub fn get_transactions(&self, root: Hash) -> BoxFuture<TxnBatch> {
        let (span, options) = self.prepare_options("TxnClient::get_transactions");
        let mut request = api::client::GetTransactionsRequest::new();
        request.set_runtime_id(self.runtime_id.as_ref().to_vec());
        request.set_root(root.as_ref().to_vec());

        let result: BoxFuture<TxnBatch> =
            match self.client.get_transactions_async_opt(&request, options) {
                Ok(resp) => Box::new(
                    resp.map_err(|error| TxnClientError::CallFailed(format!("{}", error)).into())
                        .map(|mut rsp| TxnBatch(rsp.take_txns().into())),
                ),
                Err(error) => Box::new(future::err(
                    TxnClientError::CallFailed(format!("{}", error)).into(),
                )),
            };
        drop(span);
        result
    }

    /// Query the block index.
    pub fn query_block<K, V>(&self, key: K, value: V) -> BoxFuture<BlockSnapshot>
    where
        K: AsRef<[u8]>,
        V: AsRef<[u8]>,
    {
        let (span, options) = self.prepare_options("TxnClient::query_block");
        let mut request = api::client::QueryBlockRequest::new();
        request.set_runtime_id(self.runtime_id.as_ref().to_vec());
        request.set_key(key.as_ref().into());
        request.set_value(value.as_ref().into());

        let result: BoxFuture<BlockSnapshot> = match self
            .client
            .query_block_async_opt(&request, options)
        {
            Ok(resp) => {
                let storage_client = self.storage_client.clone();
                Box::new(
                    resp.map_err(|error| TxnClientError::CallFailed(format!("{}", error)).into())
                        .and_then(move |rsp| {
                            Ok(BlockSnapshot::new(
                                storage_client,
                                serde_cbor::from_slice(&rsp.block)?,
                                Hash::from(rsp.block_hash),
                            ))
                        }),
                )
            }
            Err(error) => Box::new(future::err(
                TxnClientError::CallFailed(format!("{}", error)).into(),
            )),
        };
        drop(span);
        result
    }

    /// Query the transaction index.
    pub fn query_txn<K, V>(&self, key: K, value: V) -> BoxFuture<TransactionSnapshot>
    where
        K: AsRef<[u8]>,
        V: AsRef<[u8]>,
    {
        let (span, options) = self.prepare_options("TxnClient::query_txn");
        let mut request = api::client::QueryTxnRequest::new();
        request.set_runtime_id(self.runtime_id.as_ref().to_vec());
        request.set_key(key.as_ref().into());
        request.set_value(value.as_ref().into());

        let result: BoxFuture<TransactionSnapshot> = match self
            .client
            .query_txn_async_opt(&request, options)
        {
            Ok(resp) => {
                let storage_client = self.storage_client.clone();
                Box::new(
                    resp.map_err(|error| TxnClientError::CallFailed(format!("{}", error)).into())
                        .and_then(move |mut rsp| {
                            TransactionSnapshot::new(
                                storage_client,
                                serde_cbor::from_slice(&rsp.block)?,
                                Hash::from(rsp.take_block_hash()),
                                rsp.txn_index,
                                rsp.take_input(),
                                rsp.take_output(),
                            )
                        }),
                )
            }
            Err(error) => Box::new(future::err(
                TxnClientError::CallFailed(format!("{}", error)).into(),
            )),
        };
        drop(span);
        result
    }

    fn prepare_options(&self, span_name: &'static str) -> (Span, grpcio::CallOption) {
        // TODO: Use ekiden_tracing to get the tracer.
        let (tracer, _) = Tracer::new(AllSampler);

        let span = tracer
            .span(span_name)
            .tag(tag::StdTag::span_kind("client"))
            .start();

        let mut options = grpcio::CallOption::default();
        if let Some(timeout) = self.timeout {
            options = options.timeout(timeout);
        }

        // TODO: Inject to options.
        // options = inject_to_options(options, span.context());

        (span, options)
    }
}

/// Parse runtime call output.
pub fn parse_call_output<O>(output: Vec<u8>) -> Fallible<O>
where
    O: DeserializeOwned,
{
    let output: TxnOutput = serde_cbor::from_slice(&output)?;
    match output {
        TxnOutput::Success(data) => Ok(serde_cbor::from_value(data)?),
        TxnOutput::Error(error) => Err(TxnClientError::TxnFailed(error).into()),
    }
}