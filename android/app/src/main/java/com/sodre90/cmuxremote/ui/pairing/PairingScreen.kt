package com.sodre90.cmuxremote.ui.pairing

import android.Manifest
import android.content.pm.PackageManager
import androidx.activity.compose.rememberLauncherForActivityResult
import androidx.activity.result.contract.ActivityResultContracts
import androidx.camera.core.CameraSelector
import androidx.camera.core.ExperimentalGetImage
import androidx.camera.core.ImageAnalysis
import androidx.camera.core.ImageProxy
import androidx.camera.core.Preview
import androidx.camera.lifecycle.ProcessCameraProvider
import androidx.camera.view.PreviewView
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.padding
import androidx.compose.material3.Button
import androidx.compose.material3.ExperimentalMaterial3Api
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.OutlinedTextField
import androidx.compose.material3.Scaffold
import androidx.compose.material3.Text
import androidx.compose.material3.TextButton
import androidx.compose.material3.TopAppBar
import androidx.compose.runtime.Composable
import androidx.compose.runtime.DisposableEffect
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.runtime.collectAsState
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.setValue
import androidx.compose.ui.Modifier
import androidx.compose.ui.platform.LocalContext
import androidx.compose.ui.res.stringResource
import androidx.compose.ui.unit.dp
import androidx.compose.ui.viewinterop.AndroidView
import androidx.core.content.ContextCompat
import androidx.lifecycle.compose.LocalLifecycleOwner
import com.google.mlkit.vision.barcode.BarcodeScanner
import com.google.mlkit.vision.barcode.BarcodeScannerOptions
import com.google.mlkit.vision.barcode.BarcodeScanning
import com.google.mlkit.vision.barcode.common.Barcode
import com.google.mlkit.vision.common.InputImage
import com.sodre90.cmuxremote.R

@OptIn(ExperimentalMaterial3Api::class)
@Composable
fun PairingScreen(vm: PairingViewModel, title: String, onPaired: () -> Unit) {
    val context = LocalContext.current

    var hasCameraPermission by remember {
        mutableStateOf(
            ContextCompat.checkSelfPermission(context, Manifest.permission.CAMERA) ==
                PackageManager.PERMISSION_GRANTED,
        )
    }
    val permissionLauncher = rememberLauncherForActivityResult(
        ActivityResultContracts.RequestPermission(),
    ) { granted -> hasCameraPermission = granted }

    LaunchedEffect(Unit) {
        if (!hasCameraPermission) permissionLauncher.launch(Manifest.permission.CAMERA)
    }

    val state by vm.state.collectAsState()

    Scaffold(topBar = { TopAppBar(title = { Text(title) }) }) { inner ->
        Column(
            modifier = Modifier.fillMaxSize().padding(inner),
            verticalArrangement = Arrangement.Top,
        ) {
            when (val s = state) {
                is PairingUiState.Scanning -> {
                    var showManualEntry by remember { mutableStateOf(false) }
                    if (showManualEntry) {
                        ManualEntryForm(
                            onSubmit = vm::onManualEntrySubmitted,
                            onScanInstead = { showManualEntry = false },
                        )
                    } else {
                        Column(modifier = Modifier.fillMaxSize()) {
                            Box(modifier = Modifier.weight(1f)) {
                                if (hasCameraPermission) {
                                    CameraPreview(onQrDetected = vm::onQrScanned)
                                } else {
                                    Column(
                                        modifier = Modifier.padding(16.dp),
                                        verticalArrangement = Arrangement.spacedBy(12.dp),
                                    ) {
                                        Text(stringResource(R.string.pairing_camera_permission_required))
                                        Button(onClick = { permissionLauncher.launch(Manifest.permission.CAMERA) }) {
                                            Text(stringResource(R.string.pairing_grant_camera_permission))
                                        }
                                    }
                                }
                            }
                            TextButton(
                                onClick = { showManualEntry = true },
                                modifier = Modifier.fillMaxWidth(),
                            ) { Text(stringResource(R.string.pairing_enter_manually)) }
                        }
                    }
                }
                is PairingUiState.AwaitingConfirmation -> Column(
                    modifier = Modifier.padding(16.dp),
                    verticalArrangement = Arrangement.spacedBy(12.dp),
                ) {
                    Text(stringResource(R.string.pairing_confirm_fingerprint_title))
                    Text(stringResource(R.string.pairing_confirm_fingerprint_body))
                    Text(s.fingerprint, style = MaterialTheme.typography.headlineSmall)
                    Button(onClick = vm::onConfirmed, modifier = Modifier.fillMaxWidth()) {
                        Text(stringResource(R.string.action_confirm))
                    }
                    TextButton(onClick = vm::onRejected, modifier = Modifier.fillMaxWidth()) {
                        Text(stringResource(R.string.action_cancel))
                    }
                }
                is PairingUiState.Pairing ->
                    Text(stringResource(R.string.pairing_in_progress), modifier = Modifier.padding(16.dp))
                is PairingUiState.AwaitingOperator -> Column(
                    modifier = Modifier.padding(16.dp),
                    verticalArrangement = Arrangement.spacedBy(12.dp),
                ) {
                    Text(stringResource(R.string.pairing_awaiting_operator_title))
                    Text(s.fingerprint, style = MaterialTheme.typography.headlineSmall)
                    Text(stringResource(R.string.pairing_awaiting_operator_body))
                }
                is PairingUiState.Error -> Column(
                    modifier = Modifier.padding(16.dp),
                    verticalArrangement = Arrangement.spacedBy(12.dp),
                ) {
                    Text(s.message)
                    Button(onClick = vm::retry) { Text(stringResource(R.string.pairing_scan_again)) }
                }
                is PairingUiState.Success -> Column(
                    modifier = Modifier.padding(16.dp),
                    verticalArrangement = Arrangement.spacedBy(12.dp),
                ) {
                    Text(stringResource(R.string.pairing_success_title))
                    Text(s.fingerprint, style = MaterialTheme.typography.headlineSmall)
                    Text(stringResource(R.string.pairing_success_body))
                    Button(onClick = onPaired, modifier = Modifier.fillMaxWidth()) {
                        Text(stringResource(R.string.action_done))
                    }
                }
            }
        }
    }
}

/** Fallback pairing path for when scanning isn't possible (no camera, or
 *  pairing remotely, e.g. over SSH): the server URL and the pairing code
 *  `term-bridge pair-device` also prints alongside the QR are enough to
 *  complete the same handshake (see PairingClient.resolveManualCode). */
@Composable
private fun ManualEntryForm(onSubmit: (serverUrl: String, code: String) -> Unit, onScanInstead: () -> Unit) {
    var serverUrl by remember { mutableStateOf("") }
    var code by remember { mutableStateOf("") }
    Column(
        modifier = Modifier.fillMaxSize().padding(16.dp),
        verticalArrangement = Arrangement.spacedBy(12.dp),
    ) {
        Text(stringResource(R.string.pairing_manual_intro))
        OutlinedTextField(
            value = serverUrl,
            onValueChange = { serverUrl = it },
            label = { Text(stringResource(R.string.pairing_server_url_label)) },
            placeholder = { Text(stringResource(R.string.pairing_server_url_placeholder)) },
            singleLine = true,
            modifier = Modifier.fillMaxWidth(),
        )
        OutlinedTextField(
            value = code,
            onValueChange = { code = it },
            label = { Text(stringResource(R.string.pairing_code_label)) },
            singleLine = true,
            modifier = Modifier.fillMaxWidth(),
        )
        Button(
            onClick = { onSubmit(serverUrl, code) },
            enabled = serverUrl.isNotBlank() && code.isNotBlank(),
            modifier = Modifier.fillMaxWidth(),
        ) { Text(stringResource(R.string.action_pair)) }
        TextButton(onClick = onScanInstead, modifier = Modifier.fillMaxWidth()) {
            Text(stringResource(R.string.pairing_scan_qr_instead))
        }
    }
}

@Composable
private fun CameraPreview(onQrDetected: (String) -> Unit) {
    val lifecycleOwner = LocalLifecycleOwner.current
    val scanner = remember {
        BarcodeScanning.getClient(
            BarcodeScannerOptions.Builder().setBarcodeFormats(Barcode.FORMAT_QR_CODE).build(),
        )
    }
    // Bound once resolved (async); unbound in onDispose below so the camera is
    // released as soon as this leaves composition (e.g. once pairing starts)
    // rather than staying held open for the lifetime of the whole Activity --
    // bindToLifecycle only reacts to lifecycleOwner's ON_STOP/ON_DESTROY, not
    // to this composable being removed from the tree.
    var boundCameraProvider by remember { mutableStateOf<ProcessCameraProvider?>(null) }

    DisposableEffect(Unit) {
        onDispose { boundCameraProvider?.unbindAll() }
    }

    AndroidView(
        modifier = Modifier.fillMaxSize(),
        factory = { ctx ->
            val previewView = PreviewView(ctx)
            val cameraProviderFuture = ProcessCameraProvider.getInstance(ctx)
            cameraProviderFuture.addListener(
                {
                    val cameraProvider = cameraProviderFuture.get()
                    val preview = Preview.Builder().build().also {
                        it.surfaceProvider = previewView.surfaceProvider
                    }
                    val analysis = ImageAnalysis.Builder().build().also { analysis ->
                        analysis.setAnalyzer(ContextCompat.getMainExecutor(ctx)) { imageProxy ->
                            processImageProxy(scanner, imageProxy, onQrDetected)
                        }
                    }
                    cameraProvider.unbindAll()
                    cameraProvider.bindToLifecycle(
                        lifecycleOwner,
                        CameraSelector.DEFAULT_BACK_CAMERA,
                        preview,
                        analysis,
                    )
                    boundCameraProvider = cameraProvider
                },
                ContextCompat.getMainExecutor(ctx),
            )
            previewView
        },
    )
}

@androidx.annotation.OptIn(ExperimentalGetImage::class)
private fun processImageProxy(scanner: BarcodeScanner, imageProxy: ImageProxy, onQrDetected: (String) -> Unit) {
    val mediaImage = imageProxy.image
    if (mediaImage == null) {
        imageProxy.close()
        return
    }
    val image = InputImage.fromMediaImage(mediaImage, imageProxy.imageInfo.rotationDegrees)
    scanner.process(image)
        .addOnSuccessListener { barcodes -> barcodes.firstOrNull()?.rawValue?.let(onQrDetected) }
        .addOnCompleteListener { imageProxy.close() }
}
