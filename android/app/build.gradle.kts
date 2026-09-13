import java.io.File
import org.jetbrains.kotlin.gradle.dsl.JvmTarget

plugins {
    alias(libs.plugins.android.application)
    alias(libs.plugins.kotlin.android)
    alias(libs.plugins.kotlin.compose)
    alias(libs.plugins.kotlin.serialization)
    alias(libs.plugins.detekt)
}

// Apply the Firebase google-services plugin only when the config file exists, so
// the app still builds without push configured.
if (File(projectDir, "google-services.json").exists()) {
    apply(plugin = "com.google.gms.google-services")
}

android {
    namespace = "com.sodre90.cmuxremote"
    compileSdk = 36

    defaultConfig {
        applicationId = "com.sodre90.cmuxremote"
        minSdk = 26
        targetSdk = 35
        versionCode = 9
        versionName = "0.8.0"
        testInstrumentationRunner = "androidx.test.runner.AndroidJUnitRunner"
    }

    buildTypes {
        release {
            isMinifyEnabled = true
            proguardFiles(
                getDefaultProguardFile("proguard-android-optimize.txt"),
                "proguard-rules.pro",
            )
        }
    }

    compileOptions {
        sourceCompatibility = JavaVersion.VERSION_17
        targetCompatibility = JavaVersion.VERSION_17
    }
    buildFeatures {
        compose = true
        buildConfig = true
    }
    packaging {
        resources {
            excludes += "META-INF/versions/9/OSGI-INF/MANIFEST.MF"
        }
    }
    testOptions {
        unitTests {
            isIncludeAndroidResources = true
            // android.util.Log is a stub in unit tests and throws when called,
            // so any test exercising a code path that logs used to fail on the
            // logging rather than on what it was testing -- which is what kept
            // TerminalSocket's frame-failure path untested. Defaults make those
            // calls no-ops.
            isReturnDefaultValues = true
        }
    }
}

kotlin {
    compilerOptions {
        jvmTarget.set(JvmTarget.JVM_17)
    }
}

// Formatting-only detekt run (see config/detekt/detekt.yml for why the rest
// of the default ruleset is switched off) plus the formatting rule set
// (detekt-formatting dependency below).
detekt {
    buildUponDefaultConfig = true
    autoCorrect = false
    config.setFrom(files("$rootDir/config/detekt/detekt.yml"))
}

dependencies {
    detektPlugins(libs.detekt.formatting)

    implementation(libs.androidx.core.ktx)
    implementation(libs.androidx.lifecycle.runtime.ktx)
    implementation(libs.androidx.lifecycle.viewmodel.compose)
    implementation(libs.androidx.lifecycle.runtime.compose)
    implementation(libs.androidx.work.runtime.ktx)
    implementation(libs.androidx.activity.compose)
    implementation(platform(libs.androidx.compose.bom))
    implementation(libs.androidx.ui)
    implementation(libs.androidx.ui.graphics)
    implementation(libs.androidx.ui.tooling.preview)
    implementation(libs.androidx.material3)
    implementation(libs.androidx.navigation.compose)
    implementation(libs.androidx.material.icons.core)
    implementation(libs.androidx.exifinterface)
    implementation(libs.reorderable)
    implementation(libs.okhttp)
    implementation(libs.kotlinx.serialization.json)
    implementation(libs.kotlinx.coroutines.android)
    implementation(libs.androidx.security.crypto)
    implementation(platform(libs.firebase.bom))
    implementation(libs.firebase.messaging)
    implementation(libs.bouncycastle)
    implementation(libs.lazysodium.android) {
        exclude(group = "net.java.dev.jna", module = "jna")
    }
    implementation(libs.jna) { artifact { type = "aar" } }
    implementation(libs.androidx.camera.core)
    implementation(libs.androidx.camera.camera2)
    implementation(libs.androidx.camera.lifecycle)
    implementation(libs.androidx.camera.view)
    implementation(libs.mlkit.barcode.scanning)

    debugImplementation(libs.androidx.ui.tooling)
    // Not testImplementation: this contributes the bare ComponentActivity that
    // createComposeRule() launches, and a Robolectric test reads it from the
    // merged DEBUG manifest.
    debugImplementation(libs.androidx.ui.test.manifest)

    testImplementation(libs.junit)
    testImplementation(libs.okhttp.mockwebserver)
    testImplementation(libs.okhttp.tls)
    testImplementation(libs.kotlinx.serialization.json)
    testImplementation(libs.kotlinx.coroutines.test)
    testImplementation(libs.lazysodium.java)
    testImplementation(libs.jna)
    testImplementation(libs.robolectric)
    testImplementation(libs.androidx.navigation.testing)
    testImplementation(platform(libs.androidx.compose.bom))
    testImplementation(libs.androidx.ui.test.junit4)
}

val lazysodiumNativeLibDir = layout.buildDirectory.dir("native-libs/lazysodium")

val extractLazysodiumNativeLib by tasks.registering(Copy::class) {
    val lazysodiumJar = configurations.detachedConfiguration(
        dependencies.create("com.goterl:lazysodium-java:${libs.versions.lazysodium.get()}"),
    ).resolve().single { it.name.startsWith("lazysodium-java") }

    val osName = System.getProperty("os.name").lowercase()
    val osArch = System.getProperty("os.arch").lowercase()
    val resourceDir = when {
        osName.contains("mac") && (osArch == "aarch64" || osArch == "arm64") -> "mac_arm"
        osName.contains("mac") -> "mac"
        osName.contains("linux") -> "linux64"
        osName.contains("windows") -> "windows64"
        else -> error("lazysodium-java: no bundled native library known for os.name=$osName os.arch=$osArch")
    }
    val libFileName = when {
        osName.contains("windows") -> "libsodium.dll"
        osName.contains("mac") -> "libsodium.dylib"
        else -> "libsodium.so"
    }

    from(zipTree(lazysodiumJar)) {
        include("$resourceDir/$libFileName")
    }
    into(lazysodiumNativeLibDir)
    eachFile { path = name }
    includeEmptyDirs = false
}

tasks.withType<Test>().configureEach {
    dependsOn(extractLazysodiumNativeLib)
    systemProperty("jna.library.path", lazysodiumNativeLibDir.get().asFile.absolutePath)
}
