import Foundation

struct NetworkFilterDaemonAllowRule: Decodable {
    let host: String
    let ports: [Int]
    let remoteIPs: [String]?

    private enum CodingKeys: String, CodingKey {
        case host
        case ports
        case remoteIPs = "remote_ips"
    }
}

struct NetworkFilterDaemonProcessRule: Decodable {
    let pid: Int32
    let allow: [NetworkFilterDaemonAllowRule]
}

struct NetworkFilterDaemonGuestRule: Decodable {
    let guestIP: String
    let defaultAction: String?
    let allowDNS: Bool?
    let allow: [NetworkFilterDaemonAllowRule]

    private enum CodingKeys: String, CodingKey {
        case guestIP = "guest_ip"
        case defaultAction = "default_action"
        case allowDNS = "allow_dns"
        case allow
    }
}

struct NetworkFilterDaemonPolicySnapshot: Decodable {
    let version: Int
    let updatedAt: String
    let defaultAction: String
    let targetProcessPath: String?
    let allow: [NetworkFilterDaemonAllowRule]
    let guestRules: [NetworkFilterDaemonGuestRule]?
    let processRules: [NetworkFilterDaemonProcessRule]?

    private enum CodingKeys: String, CodingKey {
        case version
        case updatedAt = "updated_at"
        case defaultAction = "default_action"
        case targetProcessPath = "target_process_path"
        case allow
        case guestRules = "guest_rules"
        case processRules = "process_rules"
    }
}

struct NetworkFilterDaemonStatusSnapshot: Decodable {
    let version: Int
    let updatedAt: String?
    let available: Bool
    let loaded: Bool
    let enabled: Bool
    let configured: Bool
    let lastError: String?
    let providerStartedAt: String?
    let providerUpdatedAt: String?
    let providerLastError: String?

    private enum CodingKeys: String, CodingKey {
        case version
        case updatedAt = "updated_at"
        case available
        case loaded
        case enabled
        case configured
        case lastError = "last_error"
        case providerStartedAt = "provider_started_at"
        case providerUpdatedAt = "provider_updated_at"
        case providerLastError = "provider_last_error"
    }
}

enum NetworkFilterDaemonError: LocalizedError {
    case invalidBaseURL(String)
    case requestTimedOut
    case requestFailed(String)
    case serverError(String)
    case invalidResponse
    case decodeFailed(String)

    var errorDescription: String? {
        switch self {
        case .invalidBaseURL(let value):
            return "invalid network filter daemon URL: \(value)"
        case .requestTimedOut:
            return "network filter daemon request timed out"
        case .requestFailed(let value):
            return value
        case .serverError(let value):
            return value
        case .invalidResponse:
            return "network filter daemon returned an invalid response"
        case .decodeFailed(let value):
            return "failed to decode network filter daemon response: \(value)"
        }
    }
}

final class NetworkFilterDaemonClient {
    static let defaultBaseURL = "http://127.0.0.1:8171"
    static let healthPath = "/healthz"
    static let policyPath = "/v1/policy"
    static let statusPath = "/v1/status"
    static let statePath = "/v1/state"

    private let baseURL: URL
    private let session: URLSession
    private let decoder = JSONDecoder()

    init(baseURLString: String = defaultBaseURL, timeout: TimeInterval = 2) throws {
        guard let baseURL = URL(string: baseURLString.trimmingCharacters(in: .whitespacesAndNewlines)) else {
            throw NetworkFilterDaemonError.invalidBaseURL(baseURLString)
        }
        self.baseURL = baseURL

        let configuration = URLSessionConfiguration.ephemeral
        configuration.timeoutIntervalForRequest = timeout
        configuration.timeoutIntervalForResource = timeout
        self.session = URLSession(configuration: configuration)
    }

    func healthCheck() throws {
        _ = try request(method: "GET", path: Self.healthPath)
    }

    func getPolicy() throws -> NetworkFilterDaemonPolicySnapshot? {
        let (data, response) = try request(method: "GET", path: Self.policyPath, allowNotFound: true)
        guard let response else {
            return nil
        }
        return try decode(NetworkFilterDaemonPolicySnapshot.self, from: data, response: response)
    }

    func getStatus() throws -> NetworkFilterDaemonStatusSnapshot? {
        let (data, response) = try request(method: "GET", path: Self.statusPath, allowNotFound: true)
        guard let response else {
            return nil
        }
        return try decode(NetworkFilterDaemonStatusSnapshot.self, from: data, response: response)
    }

    @discardableResult
    func patchStatus(_ patch: [String: Any]) throws -> NetworkFilterDaemonStatusSnapshot {
        let (data, response) = try request(method: "PATCH", path: Self.statusPath, jsonBody: patch)
        guard let response else {
            throw NetworkFilterDaemonError.invalidResponse
        }
        return try decode(NetworkFilterDaemonStatusSnapshot.self, from: data, response: response)
    }

    func reset() throws {
        _ = try request(method: "DELETE", path: Self.statePath)
    }

    private func decode<T: Decodable>(_ type: T.Type, from data: Data, response: HTTPURLResponse) throws -> T {
        do {
            return try decoder.decode(type, from: data)
        } catch {
            throw NetworkFilterDaemonError.decodeFailed(error.localizedDescription + " (\(response.statusCode))")
        }
    }

    private func request(
        method: String,
        path: String,
        jsonBody: [String: Any]? = nil,
        allowNotFound: Bool = false
    ) throws -> (Data, HTTPURLResponse?) {
        let requestURL = baseURL.appendingPathComponent(path.trimmingCharacters(in: CharacterSet(charactersIn: "/")))
        var request = URLRequest(url: requestURL)
        request.httpMethod = method
        if let jsonBody {
            request.httpBody = try JSONSerialization.data(withJSONObject: jsonBody, options: [])
            request.setValue("application/json", forHTTPHeaderField: "Content-Type")
        }

        let semaphore = DispatchSemaphore(value: 0)
        var responseData: Data?
        var responseError: Error?
        var httpResponse: HTTPURLResponse?

        let task = session.dataTask(with: request) { data, response, error in
            responseData = data
            responseError = error
            httpResponse = response as? HTTPURLResponse
            semaphore.signal()
        }
        task.resume()
        semaphore.wait()

        if let responseError = responseError as NSError? {
            if responseError.domain == NSURLErrorDomain, responseError.code == NSURLErrorTimedOut {
                throw NetworkFilterDaemonError.requestTimedOut
            }
            throw NetworkFilterDaemonError.requestFailed(responseError.localizedDescription)
        }
        guard let httpResponse else {
            throw NetworkFilterDaemonError.invalidResponse
        }
        if allowNotFound, httpResponse.statusCode == 404 {
            return (Data(), nil)
        }
        guard (200...299).contains(httpResponse.statusCode) else {
            let message = responseData
                .flatMap { String(data: $0, encoding: .utf8) }?
                .trimmingCharacters(in: .whitespacesAndNewlines)
            throw NetworkFilterDaemonError.serverError(
                message?.isEmpty == false ? message! : HTTPURLResponse.localizedString(forStatusCode: httpResponse.statusCode)
            )
        }
        return (responseData ?? Data(), httpResponse)
    }
}
